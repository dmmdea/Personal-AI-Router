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
// gave a UHD 630 128 MB of DedicatedVideoMemory and PDH gave it 0 dedicated
// bytes, so the card read "VRAM 0 B / 128 MB". The row must now be a unified
// pool with the host's memory total as its ceiling and no used figure, which
// is how the same part has always been published on Linux; a discrete card
// beside it is untouched.
func TestMarkIntegratedPublishesThePool(t *testing.T) {
	const hostTotal uint64 = 51325829120 + 128<<20
	igpu := GPUInfo{Name: "Intel UHD Graphics 630 (Coffee Lake, Gen 9.5)", VramBytes: 128 << 20, statsKey: "luid_igpu"}
	markIntegrated(&igpu, hostTotal)
	if igpu.MemoryPool != noderec.GPUMemoryPoolUnified || igpu.VramBytes != hostTotal || !igpu.sharedUsageUnmeasured {
		t.Fatalf("integrated row = %+v, want a unified pool of %d with no used figure", igpu, hostTotal)
	}
	// Integrated is not "not inference-ready": that is a separate claim this
	// classification does not make.
	if igpu.InferenceReady != nil {
		t.Errorf("InferenceReady = %v, want it unset", *igpu.InferenceReady)
	}

	dgpu := GPUInfo{Name: "NVIDIA GeForce RTX 5060", VramBytes: 8279556096, statsKey: "luid_dgpu"}
	snap := statsSnapshot{GPU: map[string]gpuStat{
		"luid_igpu": {UtilizationPct: 3, VRAMUsed: 0},
		"luid_dgpu": {UtilizationPct: 71, VRAMUsed: 6209523712},
	}}
	_, raw := buildResponseDecode(t, []GPUInfo{dgpu, igpu}, nil, hostTotal, snap)
	rows, _ := raw["GPUs"].([]any)
	d, _ := rows[0].(map[string]any)
	i, _ := rows[1].(map[string]any)
	if _, present := d["memory_pool"]; present || d["vram_used_bytes"] != float64(6209523712) || d["vram_bytes"] != float64(8279556096) {
		t.Errorf("discrete row changed: %v", d)
	}
	if i["memory_pool"] != noderec.GPUMemoryPoolUnified || i["vram_bytes"] != float64(hostTotal) {
		t.Errorf("integrated row = %v, want memory_pool unified and vram_bytes %d", i, hostTotal)
	}
	if _, present := i["vram_used_bytes"]; present {
		t.Errorf("integrated row carries vram_used_bytes: %v", i)
	}
	// Same ceiling as memory.total_bytes: one figure, one base.
	mem, _ := raw["memory"].(map[string]any)
	if mem["total_bytes"] != i["vram_bytes"] {
		t.Errorf("memory.total_bytes %v != integrated ceiling %v", mem["total_bytes"], i["vram_bytes"])
	}

	// A host whose memory total could not be read keeps DXGI's figure rather
	// than a zero ceiling.
	unknown := GPUInfo{VramBytes: 128 << 20}
	markIntegrated(&unknown, 0)
	if unknown.VramBytes != 128<<20 || unknown.MemoryPool != noderec.GPUMemoryPoolUnified {
		t.Errorf("row with no host total = %+v, want DXGI's 128 MB kept, still unified", unknown)
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
