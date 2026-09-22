// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"
)

// TestRegistryLagAfterDriverReaddKeepsTemperatureOnStartupRow covers the
// window between a display-driver re-add and the DirectX registry catching up
// with the card's reissued LUID. Measured on the UHD 630 + RTX 5060 desktop
// of TestReissuedLUIDTimelineIsOneRow: the registry keys were rewritten at
// 15:30:01.8, ~14 s after the driver re-add at 15:29:48. A detection inside
// that window keeps only the iGPU — the RTX's new LUID is not
// in the registry yet, and because the kept list is not empty the
// stale-registry fallback does not fire — so the set only LOSES a key.
//
// Invalidating the temperature join on that detection would remap the card's
// PCI address to the new LUID, which has no row yet, and blank the one RTX row's
// temperature and power until the next detection. Only a detection that GAINS
// a statsKey may invalidate: the next one ({B, UHD}) re-keys the row and remaps
// the join together.
func TestRegistryLagAfterDriverReaddKeepsTemperatureOnStartupRow(t *testing.T) {
	const pci = "04:00.0"
	uhd := testAdapter("UHD", 0xf25e)
	rtxA := testAdapter("RTX", 0xf2a0)
	rtxB := testAdapter("RTX", 0x1023790)
	uhd.hardwareKey, rtxA.hardwareKey, rtxB.hardwareKey = "pci:00:02.0", "pci:"+pci, "pci:"+pci

	// The registry still holds the boot-era LUIDs (A, the iGPU, Basic Render):
	// the gate keeps the iGPU and drops the RTX's reissued LUID B.
	cands := []adapterCandidate{{gpu: rtxB, luid: 0x1023790}, {gpu: uhd, luid: 0xf25e}}
	if kept := selectPhysicalAdapters(cands, map[uint64]struct{}{0xf2a0: {}, 0xf25e: {}, 0xf223: {}}); len(kept) != 1 {
		t.Fatalf("gate kept %d, want only the iGPU", len(kept))
	}

	var stage atomic.Int32
	c := newStatsCollector(func() []GPUInfo {
		switch stage.Load() {
		case 0:
			return []GPUInfo{uhd, rtxA}
		case 1:
			return []GPUInfo{uhd} // the registry-lag window
		default:
			return []GPUInfo{rtxB, uhd}
		}
	})
	c.gpuTemps = &gpuTempPoller{}
	c.inventoryRecoverEvery = time.Millisecond
	c.inventoryRefreshEvery = time.Millisecond
	c.startGPUInventory()
	t.Cleanup(func() { close(c.stop); c.wg.Wait() })
	waitForStats(t, "the startup inventory", func() bool { return len(c.Snapshot().GPUInventory) == 2 })
	c.gpuTemps.remap.Store(false)
	stage.Store(1)
	waitForStats(t, "the registry-lag publish", func() bool { return len(c.Snapshot().GPUInventory) == 1 })
	time.Sleep(20 * time.Millisecond)
	if c.gpuTemps.remap.Load() {
		t.Fatal("a detection that only LOST a key invalidated the temperature join")
	}

	// A response inside the window: the poller was not remapped, so the
	// reading stays on the startup row (still keyed A).
	p := &gpuTempPoller{
		byAddress: map[string]string{pci: rtxA.statsKey},
		query: func() (map[string]gpuSensorSample, error) {
			return map[string]gpuSensorSample{pci: {TemperatureC: 53, PowerWatts: 12}}, nil
		},
		resolve: func() map[string]string { return map[string]string{pci: rtxB.statsKey} },
	}
	p.poll()
	snap := statsSnapshot{GPU: map[string]gpuStat{rtxB.statsKey: {VRAMUsed: 1}}, GPUInventory: []GPUInfo{uhd}}
	p.mergeInto(&snap)
	var got struct{ GPUs []GPUInfo }
	if err := json.Unmarshal(buildResponse([]GPUInfo{uhd, rtxA}, nil, 0, snap, "h", nil), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.GPUs) != 2 || got.GPUs[1].TemperatureCelsius != 53 {
		t.Fatalf("registry-lag response = %+v", got.GPUs)
	}

	stage.Store(2)
	waitForStats(t, "the reissued key's invalidation", func() bool { return c.gpuTemps.remap.Load() })
}
