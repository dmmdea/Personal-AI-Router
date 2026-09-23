// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"testing"
)

// mergeRow is the part of a merged row these tests compare.
type mergeRow struct{ name, statsKey string }

func mergeRows(gpus []GPUInfo) []mergeRow {
	out := make([]mergeRow, 0, len(gpus))
	for _, g := range gpus {
		out = append(out, mergeRow{g.Name, g.statsKey})
	}
	return out
}

func equalRows(a, b []mergeRow) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestMergeGPUInventoryFollowsReissuedKey is the measured defect: on a desktop
// with an Intel UHD 630, an RTX 5060 and a Hailo-8L, the NVIDIA driver was
// re-added two minutes after node-info started, DXGI reissued the
// RTX 5060's LUID, and the 60 s re-detect appended the card a second time
// beside the startup row. A recovered adapter at the same hardware identity as
// a startup row whose key is gone is that card: the row moves to the new key
// and keeps its place; nothing is appended.
func TestMergeGPUInventoryFollowsReissuedKey(t *testing.T) {
	uhd := GPUInfo{Name: "Intel UHD Graphics 630", statsKey: "luid_iGPU", hardwareKey: "pci:00:02.0"}
	rtxBoot := GPUInfo{Name: "NVIDIA GeForce RTX 5060", VramBytes: 8 << 30, statsKey: "luid_A", hardwareKey: "pci:04:00.0"}
	hailo := GPUInfo{Name: "Hailo-8L AI Accelerator", Kind: "npu", statsKey: "hailo:pnp:x"}
	rtxNow := rtxBoot
	rtxNow.statsKey = "luid_B"

	got := mergeGPUInventory([]GPUInfo{uhd, rtxBoot, hailo}, []GPUInfo{rtxNow, uhd}, nil)
	want := []mergeRow{
		{"Intel UHD Graphics 630", "luid_iGPU"},
		{"NVIDIA GeForce RTX 5060", "luid_B"},
		{"Hailo-8L AI Accelerator", "hailo:pnp:x"},
	}
	if !equalRows(mergeRows(got), want) {
		t.Fatalf("merged = %+v, want %+v", mergeRows(got), want)
	}

	// A second reissue (A -> B -> C) still lands on the startup row: the
	// startup list keeps the original key, and C's identity matches it.
	rtxLater := rtxBoot
	rtxLater.statsKey = "luid_C"
	got = mergeGPUInventory([]GPUInfo{uhd, rtxBoot, hailo}, []GPUInfo{rtxLater, uhd}, nil)
	if len(got) != 3 || got[1].statsKey != "luid_C" {
		t.Fatalf("second reissue merged = %+v", mergeRows(got))
	}

	// The startup slice is never mutated: the next request merges from it again.
	if rtxBoot.statsKey != "luid_A" {
		t.Fatalf("startup row mutated: %+v", rtxBoot)
	}
}

// TestMergeGPUInventoryKeepsTwoLiveAdaptersAtOneAddress pins the guard: a
// second adapter at the same hardware identity whose startup twin is STILL
// re-detected (a remoting clone that got past the registry gate) must not
// steal the live row's key — that would move the real card's readings onto
// the clone. It is appended, exactly as before the reissue rule existed.
// TestMergeGPUInventoryMovedRowTakesRedetectedIdentity: when the row moves to
// the adapter now found at its address, it shows that adapter's name and
// memory — a different card in the same slot is not reported as the old one.
func TestMergeGPUInventoryMovedRowTakesRedetectedIdentity(t *testing.T) {
	boot := GPUInfo{Name: "NVIDIA GeForce RTX 5060", VramBytes: 8 << 30, statsKey: "luid_A", hardwareKey: "pci:04:00.0"}
	swapped := GPUInfo{Name: "NVIDIA GeForce RTX 5070", VramBytes: 12 << 30, statsKey: "luid_B", hardwareKey: "pci:04:00.0"}

	got := mergeGPUInventory([]GPUInfo{boot}, []GPUInfo{swapped}, nil)
	if len(got) != 1 {
		t.Fatalf("merged %d rows, want 1: %+v", len(got), mergeRows(got))
	}
	if got[0].statsKey != "luid_B" || got[0].Name != swapped.Name || got[0].VramBytes != swapped.VramBytes {
		t.Fatalf("moved row = {%q %q %d}, want {luid_B %q %d}", got[0].statsKey, got[0].Name, got[0].VramBytes, swapped.Name, swapped.VramBytes)
	}

	// The capacity's meaning moves with it: an integrated adapter's pool
	// ceiling and its no-used-figure rule come along, and a discrete card that
	// replaces one drops them.
	igpu := GPUInfo{Name: "Intel UHD Graphics 630 (Coffee Lake, Gen 9.5)", VramBytes: 64 << 30, MemoryPool: "unified", usedIncludesShared: true, statsKey: "luid_C", hardwareKey: "pci:00:02.0"}
	got = mergeGPUInventory([]GPUInfo{boot}, []GPUInfo{{Name: igpu.Name, VramBytes: igpu.VramBytes, MemoryPool: igpu.MemoryPool, usedIncludesShared: true, statsKey: "luid_D", hardwareKey: "pci:04:00.0"}}, nil)
	if got[0].MemoryPool != "unified" || !got[0].usedIncludesShared {
		t.Errorf("row moved onto an integrated adapter = %+v, want its unified pool", got[0])
	}
	got = mergeGPUInventory([]GPUInfo{igpu}, []GPUInfo{{Name: swapped.Name, VramBytes: swapped.VramBytes, statsKey: "luid_E", hardwareKey: "pci:00:02.0"}}, nil)
	if got[0].MemoryPool != "" || got[0].usedIncludesShared {
		t.Errorf("row moved onto a discrete card = %+v, want no pool marker", got[0])
	}
}

func TestMergeGPUInventoryKeepsTwoLiveAdaptersAtOneAddress(t *testing.T) {
	card := GPUInfo{Name: "NVIDIA GeForce RTX 5080", statsKey: "luid_A", hardwareKey: "pci:01:00.0"}
	clone := GPUInfo{Name: "NVIDIA GeForce RTX 5080", statsKey: "luid_R", hardwareKey: "pci:01:00.0"}

	for _, order := range [][]GPUInfo{{card, clone}, {clone, card}} {
		got := mergeGPUInventory([]GPUInfo{card}, order, nil)
		want := []mergeRow{{"NVIDIA GeForce RTX 5080", "luid_A"}, {"NVIDIA GeForce RTX 5080", "luid_R"}}
		if !equalRows(mergeRows(got), want) {
			t.Fatalf("recovered %v: merged = %+v, want %+v", mergeRows(order), mergeRows(got), want)
		}
	}
}

// TestMergeGPUInventoryWithoutHardwareKeyIsUnchanged: platforms whose statsKey
// is already stable (Linux UUID / PCI, Darwin IORegistry) leave hardwareKey
// empty, and for them a new key is a new adapter, appended as it always was.
func TestMergeGPUInventoryWithoutHardwareKeyIsUnchanged(t *testing.T) {
	a := GPUInfo{Name: "GPU", statsKey: "uuid-a"}
	b := GPUInfo{Name: "GPU", statsKey: "uuid-b"}
	got := mergeGPUInventory([]GPUInfo{a}, []GPUInfo{b}, nil)
	want := []mergeRow{{"GPU", "uuid-a"}, {"GPU", "uuid-b"}}
	if !equalRows(mergeRows(got), want) {
		t.Fatalf("merged = %+v, want %+v", mergeRows(got), want)
	}
}

// TestMergeGPUInventoryFillsUnreadableAddressFromRedetection: a startup row
// whose PCI address could not be read at boot (hardwareKey "") takes the one
// its statsKey is re-detected with, as Name and VramBytes are filled in.
func TestMergeGPUInventoryFillsUnreadableAddressFromRedetection(t *testing.T) {
	boot := GPUInfo{Name: "NVIDIA GeForce RTX 5060", statsKey: "luid_A"}
	read := boot
	read.hardwareKey = "pci:04:00.0"

	got := mergeGPUInventory([]GPUInfo{boot}, []GPUInfo{read}, nil)
	if len(got) != 1 || got[0].statsKey != "luid_A" || got[0].hardwareKey != "pci:04:00.0" {
		t.Fatalf("merged = %+v, want the one row carrying the re-detected address", got)
	}
	if boot.hardwareKey != "" {
		t.Fatalf("startup row mutated: %+v", boot)
	}
}

// TestMergeGPUInventoryFindsUnreadableRowAfterReissue: the startup row lacked
// a hardwareKey, and by the time the card's LUID is reissued the re-detected
// list holds only the new LUID. The pairing the collector remembered from an
// earlier detection (luid_A -> pci:04:00.0) is the only link left between the
// startup row and the card, and it must move the row, not append a second.
// Without the memory the merge cannot know, and appends — which is why the
// collector keeps it.
func TestMergeGPUInventoryFindsUnreadableRowAfterReissue(t *testing.T) {
	uhd := GPUInfo{Name: "Intel UHD Graphics 630", statsKey: "luid_iGPU", hardwareKey: "pci:00:02.0"}
	rtxBoot := GPUInfo{Name: "NVIDIA GeForce RTX 5060", statsKey: "luid_A"}
	rtxNow := GPUInfo{Name: "NVIDIA GeForce RTX 5060", statsKey: "luid_B", hardwareKey: "pci:04:00.0"}
	remembered := map[string]string{"luid_iGPU": "pci:00:02.0", "luid_A": "pci:04:00.0", "luid_B": "pci:04:00.0"}

	got := mergeGPUInventory([]GPUInfo{uhd, rtxBoot}, []GPUInfo{rtxNow, uhd}, remembered)
	want := []mergeRow{{"Intel UHD Graphics 630", "luid_iGPU"}, {"NVIDIA GeForce RTX 5060", "luid_B"}}
	if !equalRows(mergeRows(got), want) {
		t.Fatalf("with the remembered pairing: merged = %+v, want %+v", mergeRows(got), want)
	}

	got = mergeGPUInventory([]GPUInfo{uhd, rtxBoot}, []GPUInfo{rtxNow, uhd}, nil)
	if len(got) != 3 {
		t.Fatalf("without any pairing for luid_A the merge matched it anyway: %+v", mergeRows(got))
	}
}

// TestMergeGPUInventoryRememberedAddressKeepsLiveGuard: a remembered pairing
// fills the startup row's hardwareKey, but a second adapter at that address
// while the startup statsKey is still re-detected is still a second adapter.
func TestMergeGPUInventoryRememberedAddressKeepsLiveGuard(t *testing.T) {
	card := GPUInfo{Name: "NVIDIA GeForce RTX 5080", statsKey: "luid_A"}
	cardNow := GPUInfo{Name: "NVIDIA GeForce RTX 5080", statsKey: "luid_A", hardwareKey: "pci:01:00.0"}
	clone := GPUInfo{Name: "NVIDIA GeForce RTX 5080", statsKey: "luid_R", hardwareKey: "pci:01:00.0"}
	remembered := map[string]string{"luid_A": "pci:01:00.0", "luid_R": "pci:01:00.0"}

	got := mergeGPUInventory([]GPUInfo{card}, []GPUInfo{clone, cardNow}, remembered)
	want := []mergeRow{{"NVIDIA GeForce RTX 5080", "luid_A"}, {"NVIDIA GeForce RTX 5080", "luid_R"}}
	if !equalRows(mergeRows(got), want) {
		t.Fatalf("merged = %+v, want %+v", mergeRows(got), want)
	}
}

// TestBuildResponseReissuedKeyIsOneRow is the wire-level form of the defect:
// with every live reading keyed by the reissued LUID, the response lists the
// card once, carrying VRAM used AND temperature/power on the same row.
func TestBuildResponseReissuedKeyIsOneRow(t *testing.T) {
	rtxBoot := GPUInfo{Name: "NVIDIA GeForce RTX 5060", VramBytes: 8279556096, statsKey: "luid_A", hardwareKey: "pci:04:00.0"}
	rtxNow := rtxBoot
	rtxNow.statsKey = "luid_B"
	snap := statsSnapshot{
		GPU:          map[string]gpuStat{"luid_B": {VRAMUsed: 422301696, TemperatureC: 56, PowerWatts: 19}},
		GPUInventory: []GPUInfo{rtxNow},
	}
	var got struct {
		GPUs []GPUInfo `json:"GPUs"`
	}
	if err := json.Unmarshal(buildResponse([]GPUInfo{rtxBoot}, nil, 0, snap, "host", nil), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.GPUs) != 1 {
		t.Fatalf("response listed %d GPUs, want 1: %+v", len(got.GPUs), got.GPUs)
	}
	g := got.GPUs[0]
	if g.VramUsedBytes != 422301696 || g.TemperatureCelsius != 56 || g.PowerWatts != 19 {
		t.Fatalf("row = %+v, want VRAM used + temperature + power on the one row", g)
	}
}

// TestMergeGPUInventoryAdoptsALaterIntegratedClassification: when the OS could
// not classify an adapter at boot (DXCore unanswered), the startup row is
// discrete. A re-detection that does classify it integrated must reach the
// response under the same statsKey, pool ceiling included; a later pass that
// again cannot say must not undo it.
func TestMergeGPUInventoryAdoptsALaterIntegratedClassification(t *testing.T) {
	boot := GPUInfo{Name: "Intel Graphics (device 0xffff)", VramBytes: 128 << 20, statsKey: "luid_A", hardwareKey: "pci:00:02.0"}
	classified := GPUInfo{Name: boot.Name, VramBytes: 48 << 30, MemoryPool: "unified", usedIncludesShared: true, statsKey: "luid_A", hardwareKey: "pci:00:02.0"}

	got := mergeGPUInventory([]GPUInfo{boot}, []GPUInfo{classified}, nil)
	if len(got) != 1 || got[0].MemoryPool != "unified" || !got[0].usedIncludesShared || got[0].VramBytes != 48<<30 {
		t.Fatalf("merged = %+v, want the startup row to take the unified pool and its ceiling", mergeRows(got))
	}

	unifiedBoot := classified
	unanswered := boot
	got = mergeGPUInventory([]GPUInfo{unifiedBoot}, []GPUInfo{unanswered}, nil)
	if got[0].MemoryPool != "unified" || got[0].VramBytes != 48<<30 {
		t.Fatalf("merged = %+v, want a later unanswered pass to leave the unified row alone", mergeRows(got))
	}
}
