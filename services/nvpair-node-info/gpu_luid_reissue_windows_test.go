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

// TestGPUTempPollerFollowsReissuedLUID: the poller's address map resolved the
// card's PCI address to its startup LUID. After a driver reinstall the address
// is unchanged, so the unknown-address re-resolve never fires and the readings
// kept landing on the dead LUID. invalidate must make the next poll re-resolve,
// and without it the known address must keep its mapping (no per-poll DXGI).
func TestGPUTempPollerFollowsReissuedLUID(t *testing.T) {
	const pci = "04:00.0"
	current := map[string]string{pci: "luid_B"}
	var resolves atomic.Int64
	p := &gpuTempPoller{
		byAddress: map[string]string{pci: "luid_A"},
		query: func() (map[string]gpuSensorSample, error) {
			return map[string]gpuSensorSample{pci: {TemperatureC: 56, PowerWatts: 19}}, nil
		},
		resolve: func() map[string]string { resolves.Add(1); return current },
	}
	published := func() map[string]gpuSensorSample {
		if m := p.latest.Load(); m != nil {
			return *m
		}
		return nil
	}

	p.poll()
	if _, ok := published()["luid_A"]; !ok || resolves.Load() != 0 {
		t.Fatalf("before invalidate: published %v after %d resolves; a known address must not re-resolve", published(), resolves.Load())
	}

	p.invalidate()
	p.poll()
	if got := published(); got["luid_B"].TemperatureC != 56 || len(got) != 1 {
		t.Fatalf("after invalidate: published %v, want the reading on luid_B only", got)
	}
	if resolves.Load() != 1 {
		t.Fatalf("resolves = %d, want exactly 1", resolves.Load())
	}
	p.poll()
	if resolves.Load() != 1 {
		t.Fatalf("invalidate re-resolved more than once: %d", resolves.Load())
	}

	var none *gpuTempPoller
	none.invalidate() // nil-safe: a collector without a poller calls it too
}

// TestWindowsCollectorInvalidatesTempJoinOnChangedSet: the inventory loop is
// what notices a reissued LUID (its statsKey set changes), so it must tell the
// temperature poller — and must not when the set is unchanged.
func TestWindowsCollectorInvalidatesTempJoinOnChangedSet(t *testing.T) {
	boot := []GPUInfo{testAdapter("NVIDIA GeForce RTX 5060", 0xf2a0)}
	after := []GPUInfo{testAdapter("NVIDIA GeForce RTX 5060", 0x1023790)}
	var reinstalled atomic.Bool
	var calls atomic.Int64
	c := newStatsCollector(func() []GPUInfo {
		calls.Add(1)
		if reinstalled.Load() {
			return after
		}
		return boot
	})
	c.gpuTemps = &gpuTempPoller{}
	c.inventoryRecoverEvery = time.Millisecond
	c.inventoryRefreshEvery = time.Millisecond
	c.startGPUInventory()
	t.Cleanup(func() { close(c.stop); c.wg.Wait() })

	waitForStats(t, "the startup inventory", func() bool { return len(c.Snapshot().GPUInventory) == 1 })
	c.gpuTemps.remap.Store(false)
	settled := calls.Load() + 3
	waitForStats(t, "three unchanged detections", func() bool { return calls.Load() >= settled })
	if c.gpuTemps.remap.Load() {
		t.Fatal("an unchanged adapter set invalidated the temperature join")
	}

	reinstalled.Store(true)
	waitForStats(t, "the invalidation", func() bool { return c.gpuTemps.remap.Load() })
}

// TestReissuedLUIDTimelineIsOneRow replays a measured timeline (a desktop with
// an Intel UHD 630, an RTX 5060 and a Hailo-8L) end to end through the
// production pieces. node-info started at 15:27:44 and saw the
// RTX 5060 under LUID A; the NVIDIA driver was re-added at 15:29:48 and DXGI
// now reports LUID 0x1023790 for the same PCI address 04:00.0. Before the fix
// this produced, verbatim, the live four-row response: the startup row with
// nvidia-smi temperature/power, the Hailo row, and an appended RTX row with
// only the PDH VRAM figure. After it: one RTX row, in its startup position,
// carrying all three readings.
func TestReissuedLUIDTimelineIsOneRow(t *testing.T) {
	const pci = "04:00.0"
	uhd := GPUInfo{Name: "Intel UHD Graphics 630 (Coffee Lake, Gen 9.5)", VramBytes: 134217728, statsKey: luidKey(0xf25e, 0), hardwareKey: "pci:00:02.0"}
	rtxBoot := GPUInfo{Name: "NVIDIA GeForce RTX 5060", VramBytes: 8279556096, statsKey: luidKey(0xf2a0, 0), hardwareKey: "pci:" + pci}
	rtxNow := rtxBoot
	rtxNow.statsKey = luidKey(0x1023790, 0)
	hailo := GPUInfo{Name: "Hailo-8L AI Accelerator", Kind: "npu", statsKey: "hailo:pnp:x"}

	p := &gpuTempPoller{
		byAddress: map[string]string{pci: rtxBoot.statsKey}, // resolved at 15:27:44
		query: func() (map[string]gpuSensorSample, error) {
			return map[string]gpuSensorSample{pci: {TemperatureC: 56, PowerWatts: 19}}, nil
		},
		resolve: func() map[string]string {
			return map[string]string{pci: rtxNow.statsKey, "00:02.0": uhd.statsKey}
		},
	}
	p.invalidate() // the inventory loop saw {uhd, rtxNow} != {uhd, rtxBoot}
	p.poll()

	snap := statsSnapshot{
		GPU: map[string]gpuStat{ // PDH only knows live LUIDs
			uhd.statsKey:    {},
			rtxNow.statsKey: {VRAMUsed: 422301696},
			hailo.statsKey:  {TemperatureC: 49},
		},
		GPUInventory: []GPUInfo{rtxNow, uhd},
	}
	p.mergeInto(&snap)

	var got struct {
		GPUs []GPUInfo `json:"GPUs"`
	}
	if err := json.Unmarshal(buildResponse([]GPUInfo{uhd, rtxBoot, hailo}, nil, 0, snap, "host", nil), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.GPUs) != 3 {
		t.Fatalf("response listed %d rows, want 3 (UHD, RTX, Hailo): %+v", len(got.GPUs), got.GPUs)
	}
	rtx := got.GPUs[1]
	if rtx.Name != "NVIDIA GeForce RTX 5060" || rtx.VramUsedBytes != 422301696 ||
		rtx.TemperatureCelsius != 56 || rtx.PowerWatts != 19 {
		t.Fatalf("RTX row = %+v, want VRAM used, temperature and power on the one row", rtx)
	}
}

// TestReissuedLUIDFindsRowWhoseAddressWasUnreadableAtStartup: when
// D3DKMTOpenAdapterFromLuid fails for a card at startup (its display driver
// not ready yet), the startup row carries no hardwareKey, so the reissue rule
// has nothing to match it by. The address becoming readable later must reach
// the response path — the inventory loop republishes on it, and the pairing
// is remembered after the LUID it belonged to is gone — so a later reissue of
// that card still moves its row instead of appending a second one. Learning
// the address gains no statsKey, so it must not invalidate the temperature
// join; the reissue itself must.
func TestReissuedLUIDFindsRowWhoseAddressWasUnreadableAtStartup(t *testing.T) {
	const hw = "pci:04:00.0"
	rtxBoot := testAdapter("NVIDIA GeForce RTX 5060", 0xf2a0) // address unreadable: hardwareKey ""
	rtxRead := rtxBoot
	rtxRead.hardwareKey = hw
	rtxNow := testAdapter("NVIDIA GeForce RTX 5060", 0x1023790)
	rtxNow.hardwareKey = hw

	var stage atomic.Int32
	var calls atomic.Int64
	c := newStatsCollector(func() []GPUInfo {
		calls.Add(1)
		switch stage.Load() {
		case 0:
			return []GPUInfo{rtxBoot}
		case 1:
			return []GPUInfo{rtxRead}
		default:
			return []GPUInfo{rtxNow}
		}
	})
	c.gpuTemps = &gpuTempPoller{}
	c.inventoryRecoverEvery = time.Millisecond
	c.inventoryRefreshEvery = time.Millisecond
	c.startGPUInventory()
	t.Cleanup(func() { close(c.stop); c.wg.Wait() })

	waitForStats(t, "the startup inventory", func() bool { return len(c.Snapshot().GPUInventory) == 1 })
	c.gpuTemps.remap.Store(false)
	stage.Store(1)
	settled := calls.Load() + 3
	waitForStats(t, "three detections that read the address", func() bool { return calls.Load() >= settled })
	if inv := c.Snapshot().GPUInventory; len(inv) != 1 || inv[0].hardwareKey != hw {
		t.Errorf("an address that became readable was not republished: inventory %+v", inv)
	}
	if c.gpuTemps.remap.Load() {
		t.Error("learning an address (no statsKey gained) invalidated the temperature join")
	}

	stage.Store(2)
	waitForStats(t, "the reissued LUID", func() bool {
		inv := c.Snapshot().GPUInventory
		return len(inv) == 1 && inv[0].statsKey == rtxNow.statsKey
	})
	if !c.gpuTemps.remap.Load() {
		t.Error("the reissued LUID did not invalidate the temperature join")
	}

	snap := c.Snapshot()
	snap.GPU = map[string]gpuStat{rtxNow.statsKey: {VRAMUsed: 422301696, TemperatureC: 56, PowerWatts: 19}}
	var got struct {
		GPUs []GPUInfo `json:"GPUs"`
	}
	if err := json.Unmarshal(buildResponse([]GPUInfo{rtxBoot}, nil, 0, snap, "host", nil), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.GPUs) != 1 {
		t.Fatalf("response listed %d GPUs, want 1: %+v", len(got.GPUs), got.GPUs)
	}
	if g := got.GPUs[0]; g.VramUsedBytes != 422301696 || g.TemperatureCelsius != 56 || g.PowerWatts != 19 {
		t.Fatalf("row = %+v, want VRAM used, temperature and power on the one row", g)
	}
}
