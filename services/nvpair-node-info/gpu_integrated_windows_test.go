// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"os"
	"testing"

	"nvpair-shared/noderec"
)

// TestAdapterIntegrated pins the classification order: the shared PCI id table
// decides every id it lists — the same answer the Linux inventory reaches —
// and the OS is asked only about an id it does not.
func TestAdapterIntegrated(t *testing.T) {
	const (
		intel  = 0x8086
		amd    = 0x1002
		nvidia = 0x10de
	)
	cases := []struct {
		name      string
		vendor    uint32
		device    uint32
		os        func() (bool, bool)
		want      bool
		wantAskOS bool
	}{
		{name: "UHD 630 is integrated by the table", vendor: intel, device: 0x3e98, want: true},
		{name: "Tiger Lake UHD is integrated by the table", vendor: intel, device: 0x9a60, want: true},
		{name: "Arc B580 is discrete by the table", vendor: intel, device: 0xe20b, want: false},
		{name: "Barcelo APU is integrated by the table", vendor: amd, device: 0x15e7, want: true},
		// The table wins even when the OS would disagree.
		{name: "table beats the OS", vendor: intel, device: 0x3e98, os: func() (bool, bool) { return false, true }, want: true},
		{name: "unlisted Intel id asks the OS", vendor: intel, device: 0xffff, os: func() (bool, bool) { return true, true }, want: true, wantAskOS: true},
		{name: "unlisted AMD id, OS says discrete", vendor: amd, device: 0x73bf, os: func() (bool, bool) { return false, true }, want: false, wantAskOS: true},
		{name: "NVIDIA discrete is untouched", vendor: nvidia, device: 0x2d05, os: func() (bool, bool) { return false, true }, want: false, wantAskOS: true},
		{name: "NVIDIA UMA part the OS calls integrated", vendor: nvidia, device: 0x2e12, os: func() (bool, bool) { return true, true }, want: true, wantAskOS: true},
		{name: "OS cannot say keeps the row discrete", vendor: nvidia, device: 0x2d05, os: func() (bool, bool) { return true, false }, want: false, wantAskOS: true},
		{name: "no OS source keeps the row discrete", vendor: nvidia, device: 0x2d05, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			asked := false
			var osFn func() (bool, bool)
			if tc.os != nil {
				osFn = func() (bool, bool) {
					asked = true
					return tc.os()
				}
			}
			if got := adapterIntegrated(tc.vendor, tc.device, osFn); got != tc.want {
				t.Errorf("adapterIntegrated(0x%04x, 0x%04x) = %v, want %v", tc.vendor, tc.device, got, tc.want)
			}
			if tc.os != nil && asked != tc.wantAskOS {
				t.Errorf("asked the OS = %v, want %v", asked, tc.wantAskOS)
			}
		})
	}
}

// TestMarkIntegratedPublishesThePool is the defect as the wire shows it. DXGI
// gave a UHD 630 128 MB of DedicatedVideoMemory and PDH 0 dedicated bytes, so
// the card read "VRAM 0 B / 128 MB". An integrated row is now a unified pool
// whose ceiling is dedicated + shared (the audit's UHD 630: 128 MB + 47.8 GiB)
// and whose used figure is Dedicated Usage + Shared Usage (0 + 8.4 MB); a
// discrete card beside it is untouched.
func TestMarkIntegratedPublishesThePool(t *testing.T) {
	const (
		dedicated uint64 = 134217728
		shared    uint64 = 51325829120
	)
	igpu := GPUInfo{Name: "Intel UHD Graphics 630 (Coffee Lake, Gen 9.5)", VramBytes: dedicated, statsKey: "luid_igpu"}
	markIntegrated(&igpu, dedicated, shared)
	if igpu.MemoryPool != noderec.GPUMemoryPoolUnified || igpu.VramBytes != dedicated+shared || !igpu.usedIncludesShared {
		t.Fatalf("integrated row = %+v, want a unified pool of %d counting shared usage", igpu, dedicated+shared)
	}
	// Integrated is not "not inference-ready": that is a separate claim this
	// classification does not make.
	if igpu.InferenceReady != nil {
		t.Errorf("InferenceReady = %v, want it unset", *igpu.InferenceReady)
	}

	dgpu := GPUInfo{Name: "NVIDIA GeForce RTX 5060", VramBytes: 8279556096, statsKey: "luid_dgpu"}
	snap := statsSnapshot{GPU: map[string]gpuStat{
		"luid_igpu": {UtilizationPct: 3, VRAMUsed: 0, SharedUsed: 8450048, SharedUsedKnown: true},
		// A discrete card's Shared Usage (driver staging in system RAM) is
		// not part of its VRAM and must not be added to it.
		"luid_dgpu": {UtilizationPct: 71, VRAMUsed: 6209523712, SharedUsed: 146440192, SharedUsedKnown: true},
	}}
	_, raw := buildResponseDecode(t, []GPUInfo{dgpu, igpu}, nil, 0, snap)
	rows, _ := raw["GPUs"].([]any)
	d, _ := rows[0].(map[string]any)
	i, _ := rows[1].(map[string]any)
	if _, present := d["memory_pool"]; present || d["vram_used_bytes"] != float64(6209523712) || d["vram_bytes"] != float64(8279556096) {
		t.Errorf("discrete row changed: %v", d)
	}
	if i["memory_pool"] != noderec.GPUMemoryPoolUnified || i["vram_bytes"] != float64(dedicated+shared) {
		t.Errorf("integrated row = %v, want memory_pool unified and vram_bytes %d", i, dedicated+shared)
	}
	if i["vram_used_bytes"] != float64(8450048) {
		t.Errorf("integrated vram_used_bytes = %v, want dedicated 0 + shared 8450048", i["vram_used_bytes"])
	}

	// Without a Shared Usage sample the dedicated half alone would understate
	// the pool's use, so no used figure is published.
	noShared := statsSnapshot{GPU: map[string]gpuStat{"luid_igpu": {UtilizationPct: 3, VRAMUsed: 4096}}}
	_, raw = buildResponseDecode(t, []GPUInfo{igpu}, nil, 0, noShared)
	rows, _ = raw["GPUs"].([]any)
	if r, _ := rows[0].(map[string]any); r["vram_used_bytes"] != nil {
		t.Errorf("integrated row without a Shared Usage sample carries vram_used_bytes %v", r["vram_used_bytes"])
	}
}

// TestMarkIntegratedKeepsAnAPUCarveOut is the reviewer's case: a 128 GB APU
// with 96 GB set aside for graphics. GlobalMemoryStatusEx's TotalPhys excludes
// that carve-out (~32 GB), so using the host total as the ceiling would lose
// the 96 GB; dedicated + shared keeps it, and the used figure keeps counting
// the carve-out's live Dedicated Usage.
func TestMarkIntegratedKeepsAnAPUCarveOut(t *testing.T) {
	const (
		carveOut  uint64 = 96 << 30
		totalPhys uint64 = 32 << 30
		shared    uint64 = totalPhys / 2 // Windows' default shared limit
	)
	apu := GPUInfo{Name: "AMD Radeon 8060S (Strix Halo, RDNA 3.5)", VramBytes: carveOut, statsKey: "luid_apu"}
	if !adapterIntegrated(0x1002, 0x1586, nil) {
		t.Fatal("the Strix Halo id is not classified integrated")
	}
	markIntegrated(&apu, carveOut, shared)
	if apu.VramBytes != carveOut+shared {
		t.Fatalf("APU ceiling = %d, want carve-out %d + shared %d", apu.VramBytes, carveOut, shared)
	}
	if apu.VramBytes <= totalPhys {
		t.Fatalf("APU ceiling %d is no larger than TotalPhys %d: the carve-out was lost", apu.VramBytes, totalPhys)
	}
	snap := statsSnapshot{GPU: map[string]gpuStat{
		"luid_apu": {VRAMUsed: 40 << 30, SharedUsed: 1 << 30, SharedUsedKnown: true},
	}}
	typed, _ := buildResponseDecode(t, []GPUInfo{apu}, nil, totalPhys, snap)
	if got := typed.GPUs[0].VramUsedBytes; got != 41<<30 {
		t.Errorf("APU vram_used_bytes = %d, want 41 GiB (40 dedicated + 1 shared)", got)
	}
}

// TestFoldSharedUsage pins the PDH fold: readings land on the lowercased LUID
// key beside the dedicated figure, and junk instances or negative values are
// not recorded as a known 0.
func TestFoldSharedUsage(t *testing.T) {
	out := map[string]gpuStat{"luid_0x00000000_0x0000f25e_phys_0": {VRAMUsed: 7}}
	foldSharedUsage(out, map[string]int64{
		"luid_0x00000000_0x0000F25E_phys_0": 8450048,
		"luid_0x00000000_0x00001111_phys_0": -1,
		"_Total":                            5,
	})
	got := out["luid_0x00000000_0x0000f25e_phys_0"]
	if got.VRAMUsed != 7 || got.SharedUsed != 8450048 || !got.SharedUsedKnown {
		t.Errorf("folded = %+v, want dedicated kept and shared 8450048 known", got)
	}
	if len(out) != 1 {
		t.Errorf("fold recorded junk instances: %+v", out)
	}
}

// TestMemoryTotalSharesTheUsedBase pins the Windows base on this host: the
// published total is GlobalMemoryStatusEx's TotalPhys, the field used memory
// is subtracted from, so used + available == total.
func TestMemoryTotalSharesTheUsedBase(t *testing.T) {
	ms, ok := readMemoryStatus()
	if !ok {
		t.Skip("GlobalMemoryStatusEx failed on this host")
	}
	if got := detectMemoryTotal(); got != ms.TotalPhys {
		t.Fatalf("detectMemoryTotal = %d, want TotalPhys %d", got, ms.TotalPhys)
	}
	used, ok := readMemoryUsed()
	if !ok {
		t.Fatal("readMemoryUsed failed")
	}
	// Two calls, so the free figure moves between them; the base does not.
	if used > ms.TotalPhys {
		t.Fatalf("used %d exceeds the published total %d", used, ms.TotalPhys)
	}
}

// TestLiveDXCoreIntegrated runs the DXCore fallback against this host's real
// adapters. It is gated because the answer depends on the hardware; what it
// proves is that the GUIDs and vtable slots address the methods they are named
// for: the InstanceLuid read back through DXCore must equal the LUID DXGI gave,
// and IsIntegrated must answer for every hardware adapter.
func TestLiveDXCoreIntegrated(t *testing.T) {
	if os.Getenv("NVPAIR_LIVE_DXCORE") != "1" {
		t.Skip("set NVPAIR_LIVE_DXCORE=1 to query this host's adapters through DXCore")
	}
	candidates := enumerateAdapterCandidates()
	if len(candidates) == 0 {
		t.Fatal("DXGI enumerated no adapters")
	}
	for _, c := range candidates {
		luid, ok := dxcoreAdapterLUID(c.luidLow, c.luidHigh)
		if !ok {
			t.Errorf("%s: DXCore has no adapter for LUID 0x%08x:%08x", c.gpu.Name, uint32(c.luidHigh), c.luidLow)
			continue
		}
		if luid.LowPart != c.luidLow || luid.HighPart != c.luidHigh {
			t.Errorf("%s: DXCore InstanceLuid = %08x:%08x, DXGI = %08x:%08x",
				c.gpu.Name, uint32(luid.HighPart), luid.LowPart, uint32(c.luidHigh), c.luidLow)
		}
		integrated, ok := dxcoreIsIntegrated(c.luidLow, c.luidHigh)
		if !ok {
			t.Errorf("%s: DXCore IsIntegrated unanswered", c.gpu.Name)
			continue
		}
		// The row the enumeration built must agree with the classification.
		wantUnified := adapterIntegrated(c.vendorID, c.deviceID, func() (bool, bool) { return integrated, true })
		if (c.gpu.MemoryPool == noderec.GPUMemoryPoolUnified) != wantUnified {
			t.Errorf("%s: memory_pool = %q, want unified=%v", c.gpu.Name, c.gpu.MemoryPool, wantUnified)
		}
		t.Logf("%s (0x%04x:0x%04x): DXCore IsIntegrated=%v; classified integrated=%v; row memory_pool=%q vram_bytes=%d",
			c.gpu.Name, c.vendorID, c.deviceID, integrated, wantUnified, c.gpu.MemoryPool, c.gpu.VramBytes)
	}
}
