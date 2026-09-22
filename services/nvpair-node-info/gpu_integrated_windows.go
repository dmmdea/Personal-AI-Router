// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"log/slog"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/windows"

	"nvpair-shared/gpunames"
	"nvpair-shared/noderec"
)

// Integrated-adapter classification for the Windows inventory.
//
// DXGI reports every adapter's DedicatedVideoMemory, and for an integrated GPU
// that figure is the firmware's stolen aperture — 128 MB on a UHD Graphics
// 630 — while the device allocates out of shared system memory. Publishing it
// as the row's VRAM had the card read "VRAM 0 B / 128 MB" for a device whose
// reachable pool is most of the host's RAM, and the same part on Linux (where
// the row has always been a unified pool) read differently from the same part
// on Windows. An integrated row is now published the way the Linux Intel,
// Rockchip and Apple rows are: MemoryPool unified, VramBytes the host's
// memory total, and no used figure.
//
// Which adapters are integrated comes from the same shared table Linux uses
// (gpunames.IntelDiscrete / gpunames.AMDAPU), so one PCI id gets one answer on
// both platforms. An id the table does not know is asked of the OS: DXCore's
// IsIntegrated adapter property. A size heuristic is deliberately not used,
// because an AMD APU's firmware can carve 8-16 GB out of RAM and look like a
// discrete card by that measure.

// adapterIntegrated decides whether one DXGI adapter is an integrated GPU.
// osIntegrated is consulted only for an id the shared table does not list; its
// ok=false (the OS could not say) keeps the adapter discrete, which is the
// historical row shape.
func adapterIntegrated(vendorID, deviceID uint32, osIntegrated func() (integrated, ok bool)) bool {
	switch vendorID {
	case gpunames.PCIVendorIntel:
		if discrete, known := gpunames.IntelDiscrete(deviceID); known {
			return !discrete
		}
	case gpunames.PCIVendorAMD:
		if apu, known := gpunames.AMDAPU(deviceID); known {
			return apu
		}
	}
	if osIntegrated == nil {
		return false
	}
	integrated, ok := osIntegrated()
	return ok && integrated
}

// markIntegrated turns a DXGI row into a unified-pool row: the ceiling is the
// host's memory total (systemMemTotal, the same figure memory.total_bytes
// carries) and the collector's Dedicated Usage is not published against it —
// it counts only the stolen aperture, a fraction of what the device holds. A
// host whose memory total could not be read keeps the DXGI figure rather than
// publishing a zero ceiling.
func markIntegrated(gpu *GPUInfo, memTotal uint64) {
	gpu.MemoryPool = noderec.GPUMemoryPoolUnified
	gpu.sharedUsageUnmeasured = true
	if memTotal > 0 {
		gpu.VramBytes = memTotal
	}
}

// IID_IDXCoreAdapterFactory = {78ee5945-c36e-4b13-a669-005dd11c0f06}
// IID_IDXCoreAdapter        = {f0db4c7f-fe5a-42a2-bd62-f2a6cf6fc83e}
// (dxcore_interface.h, Windows SDK 10.0.26100.0)
var (
	iidIDXCoreAdapterFactory = windows.GUID{
		Data1: 0x78ee5945, Data2: 0xc36e, Data3: 0x4b13,
		Data4: [8]byte{0xa6, 0x69, 0x00, 0x5d, 0xd1, 0x1c, 0x0f, 0x06},
	}
	iidIDXCoreAdapter = windows.GUID{
		Data1: 0xf0db4c7f, Data2: 0xfe5a, Data3: 0x42a2,
		Data4: [8]byte{0xbd, 0x62, 0xf2, 0xa6, 0xcf, 0x6f, 0xc8, 0x3e},
	}

	modDXCore                      = windows.NewLazySystemDLL("dxcore.dll")
	procDXCoreCreateAdapterFactory = modDXCore.NewProc("DXCoreCreateAdapterFactory")

	dxcoreUnavailableWarned atomic.Bool
)

const (
	// IDXCoreAdapterFactory: IUnknown (0-2), CreateAdapterList(3),
	// GetAdapterByLuid(4).
	vtblIDXCoreAdapterFactoryGetAdapterByLuid = 4
	// IDXCoreAdapter: IUnknown (0-2), IsValid(3), IsAttributeSupported(4),
	// IsPropertySupported(5), GetProperty(6).
	vtblIDXCoreAdapterIsPropertySupported = 5
	vtblIDXCoreAdapterGetProperty         = 6

	// DXCoreAdapterProperty values.
	dxcorePropertyInstanceLuid = 0
	dxcorePropertyIsIntegrated = 12
)

// dxcoreLUID mirrors the Win32 LUID the DXCore calls take by reference.
type dxcoreLUID struct {
	LowPart  uint32
	HighPart int32
}

// dxcoreIsIntegrated asks DXCore whether the adapter with this LUID is an
// integrated GPU. ok is false when DXCore is absent (Windows builds before
// 2004), the adapter is unknown to it, or the property is unsupported — every
// one of which the caller treats as "cannot say".
func dxcoreIsIntegrated(luidLow uint32, luidHigh int32) (integrated, ok bool) {
	adapter := dxcoreAdapterByLUID(luidLow, luidHigh)
	if adapter == nil {
		return false, false
	}
	defer comRelease(adapter)
	if !dxcoreBool(vtableCall(adapter, vtblIDXCoreAdapterIsPropertySupported, dxcorePropertyIsIntegrated)) {
		return false, false
	}
	var value byte // DXCore's IsIntegrated is a one-byte C++ bool
	hr := vtableCall(adapter, vtblIDXCoreAdapterGetProperty,
		dxcorePropertyIsIntegrated, unsafe.Sizeof(value), uintptr(unsafe.Pointer(&value)))
	if hr != 0 {
		slog.Debug("DXCore IsIntegrated query failed", "hr", hr)
		return false, false
	}
	return value != 0, true
}

// dxcoreAdapterLUID reads the adapter's InstanceLuid back through DXCore. It
// exists for the live test, which uses the round trip to prove the vtable
// slots and the GUIDs above address the methods they are named for.
func dxcoreAdapterLUID(luidLow uint32, luidHigh int32) (dxcoreLUID, bool) {
	adapter := dxcoreAdapterByLUID(luidLow, luidHigh)
	if adapter == nil {
		return dxcoreLUID{}, false
	}
	defer comRelease(adapter)
	var out dxcoreLUID
	hr := vtableCall(adapter, vtblIDXCoreAdapterGetProperty,
		dxcorePropertyInstanceLuid, unsafe.Sizeof(out), uintptr(unsafe.Pointer(&out)))
	return out, hr == 0
}

// dxcoreAdapterByLUID returns a referenced IDXCoreAdapter for the LUID, or nil.
// The factory is created per call: DXCore hands back a process-wide singleton,
// and this runs only for an adapter the shared id table does not list.
func dxcoreAdapterByLUID(luidLow uint32, luidHigh int32) unsafe.Pointer {
	if err := procDXCoreCreateAdapterFactory.Find(); err != nil {
		if dxcoreUnavailableWarned.CompareAndSwap(false, true) {
			slog.Info("DXCore unavailable; an adapter the PCI id table does not list is reported as discrete", "err", err)
		}
		return nil
	}
	var factory unsafe.Pointer
	hr, _, _ := procDXCoreCreateAdapterFactory.Call(
		uintptr(unsafe.Pointer(&iidIDXCoreAdapterFactory)),
		uintptr(unsafe.Pointer(&factory)),
	)
	if hr != 0 || factory == nil {
		slog.Debug("DXCoreCreateAdapterFactory failed", "hr", hr)
		return nil
	}
	defer comRelease(factory)
	luid := dxcoreLUID{LowPart: luidLow, HighPart: luidHigh}
	var adapter unsafe.Pointer
	hr = vtableCall(factory, vtblIDXCoreAdapterFactoryGetAdapterByLuid,
		uintptr(unsafe.Pointer(&luid)),
		uintptr(unsafe.Pointer(&iidIDXCoreAdapter)),
		uintptr(unsafe.Pointer(&adapter)),
	)
	if hr != 0 || adapter == nil {
		slog.Debug("DXCore GetAdapterByLuid failed", "hr", hr)
		return nil
	}
	return adapter
}

// dxcoreBool reads a C++ bool return value, which occupies only the low byte
// of the return register; the upper bytes are not guaranteed to be zero.
func dxcoreBool(r uintptr) bool {
	return r&0xff != 0
}
