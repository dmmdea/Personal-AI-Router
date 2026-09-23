// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"os"
	"slices"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"

	"nvpair-shared/gpunames"
)

// TestDXGIAdapterDesc1Size pins the in-Go layout of dxgiAdapterDesc1 to the
// C ABI's DXGI_ADAPTER_DESC1 size on amd64/arm64 Windows (8-byte SIZE_T).
//
//	256  Description[128]uint16
//	  4  VendorID
//	  4  DeviceID
//	  4  SubSysID
//	  4  Revision
//	  4  (alignment padding before SIZE_T)
//	  8  DedicatedVideoMemory  (SIZE_T)
//	  8  DedicatedSystemMemory (SIZE_T)
//	  8  SharedSystemMemory    (SIZE_T)
//	  4  AdapterLuidLow
//	  4  AdapterLuidHigh
//	  4  Flags
//	  4  (trailing padding to 8-byte alignment)
//	---
//	312 bytes
//
// If this assertion fires, a field has been reordered, retyped, or the build
// has somehow targeted 32-bit Windows — none of which we want silently passing
// garbage to DXGI.
func TestDXGIAdapterDesc1Size(t *testing.T) {
	const expected = 312
	if got := unsafe.Sizeof(dxgiAdapterDesc1{}); got != expected {
		t.Fatalf("dxgiAdapterDesc1 size = %d, want %d", got, expected)
	}
}

func TestIsVirtualDisplayAdapter(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"NVIDIA GeForce RTX 5080", false},
		{"AMD Radeon RX 7900 XTX", false},
		{"Intel(R) UHD Graphics 770", false},
		{"Microsoft Remote Display Adapter", true},
		{"microsoft remote display adapter", true},
		{"REMOTE DISPLAY", true},
		{"Microsoft Basic Display Adapter", true},
		{"Basic Display something", false}, // needs "microsoft basic display"
		{"Contoso Virtual Display Adapter", true},
		{"Microsoft Hyper-V Video", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isVirtualDisplayAdapter(tc.name); got != tc.want {
			t.Errorf("isVirtualDisplayAdapter(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// fakeAdapterDesc builds a DXGI_ADAPTER_DESC1 the way DXGI fills one in: a
// UTF-16 Description (NUL-padded to 128 units) beside the PCI vendor/device
// pair. It is the whole input adapterName reads, so the name-selection rule
// is testable without a GPU, a driver or COM.
func fakeAdapterDesc(vendorID, deviceID uint32, description string) dxgiAdapterDesc1 {
	desc := dxgiAdapterDesc1{VendorID: vendorID, DeviceID: deviceID}
	copy(desc.Description[:len(desc.Description)-1], windows.StringToUTF16(description))
	return desc
}

// TestAdapterName pins the whole naming rule: an Intel or AMD adapter the
// shared table knows is published under the table's name, and everything else
// keeps DXGI's Description byte for byte. The Tiger Lake and Coffee Lake rows
// are the two measured hosts whose Windows rows used to disagree with their
// Linux counterparts.
func TestAdapterName(t *testing.T) {
	cases := []struct {
		name        string
		vendorID    uint32
		deviceID    uint32
		description string
		want        string
	}{
		{
			name:        "Intel Tiger Lake GT1 gains its generation",
			vendorID:    gpunames.PCIVendorIntel,
			deviceID:    0x9a60,
			description: "Intel(R) UHD Graphics",
			want:        "Intel UHD Graphics (Tiger Lake, Xe-LP)",
		},
		{
			name:        "Intel Coffee Lake GT2 gains its generation",
			vendorID:    gpunames.PCIVendorIntel,
			deviceID:    0x3e98,
			description: "Intel(R) UHD Graphics 630",
			want:        "Intel UHD Graphics 630 (Coffee Lake, Gen 9.5)",
		},
		{
			name:        "Intel Raptor Lake iGPU",
			vendorID:    gpunames.PCIVendorIntel,
			deviceID:    0xa780,
			description: "Intel(R) UHD Graphics 770",
			want:        "Intel UHD Graphics 770 (Raptor Lake, Xe-LP)",
		},
		{
			name:        "discrete Arc card",
			vendorID:    gpunames.PCIVendorIntel,
			deviceID:    0xe20b,
			description: "Intel(R) Arc(TM) B580 Graphics",
			want:        "Intel Arc B580 (Battlemage, Xe2-HPG)",
		},
		{
			name:        "AMD APU gains its architecture",
			vendorID:    gpunames.PCIVendorAMD,
			deviceID:    0x15e7,
			description: "AMD Radeon(TM) Graphics",
			want:        "AMD Radeon Vega Graphics (Barcelo, GCN 5.1)",
		},
		{
			name:        "unlisted Intel id keeps the DXGI description",
			vendorID:    gpunames.PCIVendorIntel,
			deviceID:    0xffff,
			description: "Intel(R) Next Graphics",
			want:        "Intel(R) Next Graphics",
		},
		{
			name:        "unlisted AMD id keeps the DXGI description",
			vendorID:    gpunames.PCIVendorAMD,
			deviceID:    0x73bf,
			description: "AMD Radeon RX 6900 XT",
			want:        "AMD Radeon RX 6900 XT",
		},
		{
			name:        "NVIDIA is never touched",
			vendorID:    0x10de,
			deviceID:    0x2c02,
			description: "NVIDIA GeForce RTX 5080",
			want:        "NVIDIA GeForce RTX 5080",
		},
		{
			// An NVIDIA device id that collides numerically with an Intel one
			// must still pass through: the vendor gate is what decides.
			name:        "NVIDIA id colliding with an Intel table entry",
			vendorID:    0x10de,
			deviceID:    0x3e98,
			description: "NVIDIA RTX A2",
			want:        "NVIDIA RTX A2",
		},
		{
			name:        "empty description stays empty",
			vendorID:    0x10de,
			deviceID:    0x2c02,
			description: "",
			want:        "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			desc := fakeAdapterDesc(tc.vendorID, tc.deviceID, tc.description)
			if got := adapterName(&desc); got != tc.want {
				t.Errorf("adapterName(%#04x:%#04x, %q) = %q, want %q",
					tc.vendorID, tc.deviceID, tc.description, got, tc.want)
			}
		})
	}
}

// TestAdapterNameNeverLosesAName is the "never worse than today" guarantee in
// test form: for a non-empty DXGI description, the published name is either
// the table's or DXGI's, never empty and never a bare device id.
func TestAdapterNameNeverLosesAName(t *testing.T) {
	const description = "Some Vendor Display Adapter"
	for _, vendorID := range []uint32{gpunames.PCIVendorIntel, gpunames.PCIVendorAMD, 0x10de, 0x1414} {
		for _, deviceID := range []uint32{0x0000, 0x3e98, 0x15e7, 0x9a60, 0xffff} {
			desc := fakeAdapterDesc(vendorID, deviceID, description)
			got := adapterName(&desc)
			if got == "" {
				t.Errorf("adapterName(%#04x:%#04x) returned an empty name", vendorID, deviceID)
			}
			if strings.Contains(got, "(device 0x") {
				t.Errorf("adapterName(%#04x:%#04x) = %q; the generic id fallback must never reach a DXGI row",
					vendorID, deviceID, got)
			}
		}
	}
}

func TestLuidUint64(t *testing.T) {
	cases := []struct {
		low  uint32
		high int32
		want uint64
	}{
		{0, 0, 0},
		{0x54f0, 0, 0x54f0},
		{0x000054f0, 0x00000001, 0x00000001000054f0},
		{0xffffffff, -1, 0xffffffffffffffff},
	}
	for _, tc := range cases {
		if got := luidUint64(tc.low, tc.high); got != tc.want {
			t.Errorf("luidUint64(%#x, %d) = %#x, want %#x", tc.low, tc.high, got, tc.want)
		}
	}
}

func TestKeepPhysicalAdapter(t *testing.T) {
	physical := map[uint64]struct{}{
		0x1000: {},
		0x2000: {},
	}
	cases := []struct {
		name     string
		luid     uint64
		physical map[uint64]struct{}
		want     bool
	}{
		{"nil map keeps all", 0x9999, nil, true},
		{"empty map keeps all", 0x9999, map[uint64]struct{}{}, true},
		{"listed LUID kept", 0x1000, physical, true},
		{"unlisted LUID dropped", 0x9999, physical, false},
		{"second listed LUID kept", 0x2000, physical, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := keepPhysicalAdapter(tc.luid, tc.physical); got != tc.want {
				t.Errorf("keepPhysicalAdapter(%#x) = %v, want %v", tc.luid, got, tc.want)
			}
		})
	}
}

// TestSelectPhysicalAdapters covers the whole-enumeration gate: it still drops
// an RDP phantom clone while the registry knows the real card, and it refuses
// to apply a registry that knows none of them — the stale state a node is in
// for the first minute after boot, which used to publish an empty GPU list
// until the service was restarted.
func TestSelectPhysicalAdapters(t *testing.T) {
	card := adapterCandidate{
		gpu:  GPUInfo{Name: "Test Adapter", statsKey: "luid_card"},
		luid: 0x1000,
	}
	clone := adapterCandidate{
		gpu:  GPUInfo{Name: "Test Adapter", statsKey: "luid_clone"},
		luid: 0x2000,
	}
	both := []adapterCandidate{card, clone}

	cases := []struct {
		name       string
		candidates []adapterCandidate
		physical   map[uint64]struct{}
		want       []string
	}{
		{
			name:       "gate keeps the listed subset",
			candidates: both,
			physical:   map[uint64]struct{}{0x1000: {}},
			want:       []string{"luid_card"},
		},
		{
			name:       "gate that would drop every adapter is skipped",
			candidates: both,
			physical:   map[uint64]struct{}{0x9999: {}, 0xaaaa: {}},
			want:       []string{"luid_card", "luid_clone"},
		},
		{
			name:       "nil map keeps all",
			candidates: both,
			physical:   nil,
			want:       []string{"luid_card", "luid_clone"},
		},
		{
			name:       "empty map keeps all",
			candidates: both,
			physical:   map[uint64]struct{}{},
			want:       []string{"luid_card", "luid_clone"},
		},
		{
			name:       "no candidates stays empty",
			candidates: nil,
			physical:   map[uint64]struct{}{0x1000: {}},
			want:       nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, c := range selectPhysicalAdapters(tc.candidates, tc.physical) {
				got = append(got, c.gpu.statsKey)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("selectPhysicalAdapters kept %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDetectGPUsLive runs only when NVPAIR_LIVE_GPU=1. On a real host,
// detection must report at least one adapter, and handing the gate a registry
// map that knows none of this boot's LUIDs — the literal stale-registry state
// — must still return every adapter rather than none.
func TestDetectGPUsLive(t *testing.T) {
	if os.Getenv("NVPAIR_LIVE_GPU") == "" {
		t.Skip("set NVPAIR_LIVE_GPU=1 to run against the host's GPUs")
	}
	gpus := detectGPUs()
	if len(gpus) == 0 {
		t.Fatal("detectGPUs reported no adapters on a host that has them")
	}
	for i, gpu := range gpus {
		t.Logf("GPU %d: %s (%d MiB, %s)", i, gpu.Name, gpu.VramBytes/(1024*1024), gpu.statsKey)
	}

	candidates := enumerateAdapterCandidates()
	t.Logf("enumerated %d candidate adapter(s), detection reported %d", len(candidates), len(gpus))
	if len(candidates) < len(gpus) {
		t.Fatalf("enumerated %d candidates but detection reported %d GPUs", len(candidates), len(gpus))
	}

	// The naming rule, checked against whatever this host actually has. On the
	// 3-card NVIDIA workstation every row must still be the driver's own
	// string; on the Tiger Lake laptop and the Coffee Lake desktop the Intel
	// row must now carry its codename and architecture, matching what the
	// Linux sysfs inventory publishes for the same silicon.
	for _, c := range candidates {
		table, listed := "", false
		switch c.vendorID {
		case gpunames.PCIVendorIntel:
			table, listed = gpunames.Intel(c.deviceID)
		case gpunames.PCIVendorAMD:
			table, listed = gpunames.AMD(c.deviceID)
		}
		t.Logf("adapter %04x:%04x dxgi=%q published=%q table_listed=%v",
			c.vendorID, c.deviceID, c.dxgiDescription, c.gpu.Name, listed)
		switch {
		case listed:
			if c.gpu.Name != table {
				t.Errorf("adapter %04x:%04x published %q, want the table name %q",
					c.vendorID, c.deviceID, c.gpu.Name, table)
			}
		default:
			if c.gpu.Name != c.dxgiDescription {
				t.Errorf("adapter %04x:%04x published %q, want DXGI's %q unchanged",
					c.vendorID, c.deviceID, c.gpu.Name, c.dxgiDescription)
			}
		}
		if c.vendorID == 0x10de && c.gpu.Name != c.dxgiDescription {
			t.Errorf("NVIDIA adapter %04x:%04x was renamed to %q; NVIDIA rows must pass through",
				c.vendorID, c.deviceID, c.gpu.Name)
		}
		if c.gpu.Name == "" {
			t.Errorf("adapter %04x:%04x published an empty name", c.vendorID, c.deviceID)
		}
	}

	stale := make(map[uint64]struct{}, len(candidates))
	for _, c := range candidates {
		stale[^c.luid] = struct{}{}
	}
	kept := selectPhysicalAdapters(candidates, stale)
	if len(kept) != len(candidates) {
		t.Fatalf("a stale registry kept %d of %d adapters; the gate is not stale-safe",
			len(kept), len(candidates))
	}
	for _, c := range kept {
		t.Logf("stale-registry gate kept %s (%s)", c.gpu.Name, c.gpu.statsKey)
	}

	fresh := make(map[uint64]struct{}, len(candidates))
	for _, c := range candidates {
		fresh[c.luid] = struct{}{}
	}
	if got := selectPhysicalAdapters(candidates, fresh); len(got) != len(candidates) {
		t.Fatalf("a registry holding this boot's LUIDs kept %d of %d adapters",
			len(got), len(candidates))
	}

	// Every reported adapter carries its PCI identity, and no two share one:
	// that identity is what keeps a card on one row when a driver reinstall
	// reissues its LUID (mergeGPUInventory), and it must agree with the address
	// the temperature join resolves for the same LUID.
	byAddress := luidsByPCIAddress()
	seen := map[string]string{}
	for _, gpu := range gpus {
		t.Logf("GPU %s hardwareKey=%q", gpu.statsKey, gpu.hardwareKey)
		if gpu.hardwareKey == "" {
			t.Errorf("%s (%s) has no PCI identity", gpu.Name, gpu.statsKey)
			continue
		}
		if other, dup := seen[gpu.hardwareKey]; dup {
			t.Errorf("%s and %s share hardwareKey %s", other, gpu.statsKey, gpu.hardwareKey)
		}
		seen[gpu.hardwareKey] = gpu.statsKey
		if addr := gpu.hardwareKey[len("pci:"):]; byAddress[addr] != gpu.statsKey {
			t.Errorf("temperature join maps %s to %q, detection says %q", addr, byAddress[addr], gpu.statsKey)
		}
	}
}
