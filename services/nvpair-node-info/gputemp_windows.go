// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows GPU temperature.
//
// PDH has no temperature counter and DXGI reports none, so the only driverless
// source on an NVIDIA host is nvidia-smi, which keys its rows by PCI bus id.
// The rest of the Windows collector keys GPUs by DXGI adapter LUID (the PDH
// instance form). The bridge between the two is the kernel-mode display
// driver: D3DKMTOpenAdapterFromLuid + D3DKMTQueryAdapterInfo
// (KMTQAITYPE_ADAPTERADDRESS) return the PCI bus/device/function behind a
// LUID, so each nvidia-smi row can be joined to its adapter without guessing
// by name — which matters on a host with two identical cards.
//
// nvidia-smi is a process spawn (~100 ms), so it runs in its own goroutine on
// a slow ticker rather than inside the 1 s PDH tick; the tick merges the
// latest published map. A host without nvidia-smi (AMD / Intel / no NVIDIA
// driver) latches unavailable on the first failure and reports no
// temperature, exactly as before.

const gpuTempPollInterval = 5 * time.Second

var (
	modGdi32                      = windows.NewLazySystemDLL("gdi32.dll")
	procD3DKMTOpenAdapterFromLuid = modGdi32.NewProc("D3DKMTOpenAdapterFromLuid")
	procD3DKMTQueryAdapterInfo    = modGdi32.NewProc("D3DKMTQueryAdapterInfo")
	procD3DKMTCloseAdapter        = modGdi32.NewProc("D3DKMTCloseAdapter")
)

// KMTQAITYPE_ADAPTERADDRESS (= 6 in KMTQUERYADAPTERINFOTYPE: UMDRIVERPRIVATE 0,
// UMDRIVERNAME 1, UMOPENGLINFO 2, GETSEGMENTSIZE 3, ADAPTERGUID 4,
// FLIPQUEUEINFO 5, ADAPTERADDRESS 6) selects D3DKMT_ADAPTERADDRESS from
// D3DKMTQueryAdapterInfo.
const kmtQueryAdapterAddress = 6

// d3dkmtOpenAdapterFromLuid mirrors D3DKMT_OPENADAPTERFROMLUID.
type d3dkmtOpenAdapterFromLuid struct {
	LuidLow  uint32
	LuidHigh int32
	HAdapter uint32
}

// d3dkmtQueryAdapterInfo mirrors D3DKMT_QUERYADAPTERINFO (amd64/arm64 layout:
// two uint32, a pointer, a uint32 + padding).
type d3dkmtQueryAdapterInfo struct {
	HAdapter              uint32
	Type                  uint32
	PrivateDriverData     unsafe.Pointer
	PrivateDriverDataSize uint32
	_                     uint32
}

// d3dkmtAdapterAddress mirrors D3DKMT_ADAPTERADDRESS.
type d3dkmtAdapterAddress struct {
	BusNumber      uint32
	DeviceNumber   uint32
	FunctionNumber uint32
}

// pciAddressKey renders a bus/device/function the way nvidia-smi prints
// pci.bus_id after its domain: lowercase "bb:dd.f".
func pciAddressKey(bus, device, function uint32) string {
	return fmt.Sprintf("%02x:%02x.%x", bus&0xff, device, function)
}

// nvidiaBusIDKey reduces an nvidia-smi pci.bus_id ("00000000:65:00.0") to the
// same "bb:dd.f" key. Returns "" when the field is not in that form.
func nvidiaBusIDKey(busID string) string {
	s := strings.ToLower(strings.TrimSpace(busID))
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return ""
	}
	bus, err := strconv.ParseUint(parts[1], 16, 32)
	if err != nil {
		return ""
	}
	devFn := strings.Split(parts[2], ".")
	if len(devFn) != 2 {
		return ""
	}
	dev, err := strconv.ParseUint(devFn[0], 16, 32)
	if err != nil {
		return ""
	}
	fn, err := strconv.ParseUint(devFn[1], 16, 32)
	if err != nil {
		return ""
	}
	return pciAddressKey(uint32(bus), uint32(dev), uint32(fn))
}

// adapterAddressForLuid asks the display driver for the PCI address behind a
// LUID. ok is false when the adapter cannot be opened or has no address (a
// software or remote adapter).
func adapterAddressForLuid(low uint32, high int32) (string, bool) {
	open := d3dkmtOpenAdapterFromLuid{LuidLow: low, LuidHigh: high}
	if st, _, _ := procD3DKMTOpenAdapterFromLuid.Call(uintptr(unsafe.Pointer(&open))); st != 0 {
		return "", false
	}
	defer func() {
		h := open.HAdapter
		procD3DKMTCloseAdapter.Call(uintptr(unsafe.Pointer(&h)))
	}()
	var addr d3dkmtAdapterAddress
	q := d3dkmtQueryAdapterInfo{
		HAdapter:              open.HAdapter,
		Type:                  kmtQueryAdapterAddress,
		PrivateDriverData:     unsafe.Pointer(&addr),
		PrivateDriverDataSize: uint32(unsafe.Sizeof(addr)),
	}
	if st, _, _ := procD3DKMTQueryAdapterInfo.Call(uintptr(unsafe.Pointer(&q))); st != 0 {
		return "", false
	}
	return pciAddressKey(addr.BusNumber, addr.DeviceNumber, addr.FunctionNumber), true
}

// luidsByPCIAddress enumerates the DXGI adapters once more (same filters as
// detectGPUs) and returns PCI address -> PDH LUID key.
func luidsByPCIAddress() map[string]string {
	var factory unsafe.Pointer
	hr, _, _ := procCreateDXGIFactory1.Call(
		uintptr(unsafe.Pointer(&iidIDXGIFactory1)),
		uintptr(unsafe.Pointer(&factory)),
	)
	if hr != 0 || factory == nil {
		return nil
	}
	defer comRelease(factory)
	out := map[string]string{}
	for i := uint32(0); ; i++ {
		adapter, hr := enumAdapters1(factory, i)
		if uint32(hr) == dxgiErrorNotFound || hr != 0 || adapter == nil {
			break
		}
		var desc dxgiAdapterDesc1
		descHR := getDesc1(adapter, &desc)
		comRelease(adapter)
		if descHR != 0 || desc.Flags&dxgiAdapterFlagSoftware != 0 {
			continue
		}
		if addr, ok := adapterAddressForLuid(desc.AdapterLuidLow, desc.AdapterLuidHigh); ok {
			out[addr] = luidKey(desc.AdapterLuidLow, desc.AdapterLuidHigh)
		}
	}
	return out
}

// nvidiaSmiTemperatures runs nvidia-smi and returns PCI address -> degrees.
func nvidiaSmiTemperatures() (map[string]uint32, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=pci.bus_id,temperature.gpu",
		"--format=csv,noheader,nounits")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return parseNvidiaTemperatures(string(out)), nil
}

// parseNvidiaTemperatures decodes "pci.bus_id, temperature.gpu" rows. Rows
// with an unparseable bus id or a non-numeric temperature ([N/A]) are skipped.
func parseNvidiaTemperatures(out string) map[string]uint32 {
	res := map[string]uint32{}
	for _, line := range strings.Split(out, "\n") {
		fields := splitCSVRow(line)
		if len(fields) < 2 {
			continue
		}
		key := nvidiaBusIDKey(fields[0])
		if key == "" {
			continue
		}
		c, err := strconv.ParseUint(fields[1], 10, 32)
		if err != nil {
			continue
		}
		res[key] = uint32(c)
	}
	return res
}

// gpuTempPoller owns the slow nvidia-smi loop and publishes LUID key ->
// degrees atomically for the PDH tick to merge.
type gpuTempPoller struct {
	byAddress   map[string]string // PCI address -> LUID key, resolved once
	latest      atomic.Pointer[map[string]uint32]
	unavailable atomic.Bool
	stop        chan struct{}
	done        chan struct{}
}

func startGPUTempPoller() *gpuTempPoller {
	p := &gpuTempPoller{
		byAddress: luidsByPCIAddress(),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
	if len(p.byAddress) == 0 {
		slog.Info("no adapter PCI addresses resolvable; GPU temperature will not be reported")
	}
	go p.run()
	return p
}

func (p *gpuTempPoller) run() {
	defer close(p.done)
	p.poll()
	ticker := time.NewTicker(gpuTempPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-ticker.C:
			p.poll()
		}
	}
}

func (p *gpuTempPoller) poll() {
	if p.unavailable.Load() || len(p.byAddress) == 0 {
		return
	}
	temps, err := nvidiaSmiTemperatures()
	if err != nil {
		if p.unavailable.CompareAndSwap(false, true) {
			slog.Warn("nvidia-smi unavailable; GPU temperature will not be reported", "err", err)
		}
		return
	}
	byLUID := make(map[string]uint32, len(temps))
	for addr, c := range temps {
		if luid, ok := p.byAddress[addr]; ok {
			byLUID[luid] = c
		}
	}
	p.latest.Store(&byLUID)
}

// mergeInto adds the latest temperatures to the snapshot's GPU map under
// their LUID keys, cloning the map first so a previously published snapshot
// (which the stale-preserve path may alias) is never mutated.
func (p *gpuTempPoller) mergeInto(snap *statsSnapshot) {
	if p == nil {
		return
	}
	temps := p.latest.Load()
	if temps == nil || len(*temps) == 0 {
		return
	}
	merged := make(map[string]gpuStat, len(snap.GPU)+len(*temps))
	for k, v := range snap.GPU {
		merged[k] = v
	}
	for luid, c := range *temps {
		s := merged[luid]
		s.TemperatureC = c
		merged[luid] = s
	}
	snap.GPU = merged
}

func (p *gpuTempPoller) Stop() {
	if p == nil {
		return
	}
	close(p.stop)
	<-p.done
}
