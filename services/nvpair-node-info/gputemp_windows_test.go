// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"os"
	"reflect"
	"testing"
	"time"
)

func TestNvidiaBusIDKey(t *testing.T) {
	cases := map[string]string{
		"00000000:65:00.0":   "65:00.0",
		"00000000:B5:00.0\n": "b5:00.0",
		"00000000:17:00.0":   "17:00.0",
		"0000:01:00.0":       "01:00.0",
		"garbage":            "",
		"":                   "",
		"00000000:zz:00.0":   "",
	}
	for in, want := range cases {
		if got := nvidiaBusIDKey(in); got != want {
			t.Errorf("nvidiaBusIDKey(%q) = %q, want %q", in, got, want)
		}
	}
	if got := pciAddressKey(0x65, 0, 0); got != "65:00.0" {
		t.Errorf("pciAddressKey = %q", got)
	}
	if got := pciAddressKey(0xb5, 3, 1); got != "b5:03.1" {
		t.Errorf("pciAddressKey = %q", got)
	}
}

// TestParseNvidiaSensors pins the decode of the one query both readings share.
// The third row is the case that matters: a card that answers [N/A] for the
// temperature but still meters itself must contribute its wattage, and the
// fourth must contribute neither rather than a cold, idle-looking pair of
// zeroes.
func TestParseNvidiaSensors(t *testing.T) {
	got := parseNvidiaSensors(
		"00000000:17:00.0, 44, 6.89\n" +
			"00000000:65:00.0, 43, 35.65\n" +
			"00000000:B5:00.0, [N/A], 120.40\n" +
			"0000:04:00.0, [N/A], [N/A]\n" +
			"broken row\n")
	want := map[string]gpuSensorSample{
		"17:00.0": {TemperatureC: 44, PowerWatts: 7},
		"65:00.0": {TemperatureC: 43, PowerWatts: 36},
		"b5:00.0": {PowerWatts: 120},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseNvidiaSensors = %v, want %v", got, want)
	}

	// A driver too old to answer power.draw sends two columns; the
	// temperature must still come through.
	twoColumn := parseNvidiaSensors("00000000:17:00.0, 44\n")
	if !reflect.DeepEqual(twoColumn, map[string]gpuSensorSample{"17:00.0": {TemperatureC: 44}}) {
		t.Fatalf("two-column row = %v", twoColumn)
	}
}

// TestGPUTempPollerMergeInto pins the merge: readings land on the LUID keys
// the PDH decoders use, existing VRAM/utilization survive, a snapshot map that
// may alias a published one is cloned rather than mutated, and a poller with
// nothing yet is a no-op.
func TestGPUTempPollerMergeInto(t *testing.T) {
	published := map[string]gpuStat{"luid_a": {VRAMUsed: 5, UtilizationPct: 40}}
	snap := &statsSnapshot{GPU: published}

	var empty *gpuTempPoller
	empty.mergeInto(snap)
	p := &gpuTempPoller{}
	p.mergeInto(snap)
	if !reflect.DeepEqual(snap.GPU, published) {
		t.Fatalf("no-op merge changed the map: %v", snap.GPU)
	}

	sensors := map[string]gpuSensorSample{
		"luid_a": {TemperatureC: 61, PowerWatts: 210},
		"luid_b": {TemperatureC: 40},
	}
	p.latest.Store(&sensors)
	p.mergeInto(snap)
	if snap.GPU["luid_a"] != (gpuStat{VRAMUsed: 5, UtilizationPct: 40, TemperatureC: 61, PowerWatts: 210}) {
		t.Fatalf("luid_a = %+v", snap.GPU["luid_a"])
	}
	if snap.GPU["luid_b"] != (gpuStat{TemperatureC: 40}) {
		t.Fatalf("luid_b = %+v", snap.GPU["luid_b"])
	}
	if _, mutated := published["luid_b"]; mutated || published["luid_a"].TemperatureC != 0 {
		t.Fatalf("published map was mutated: %v", published)
	}

	// An unmetered card must not blank a reading the rest of the collector
	// already holds: only the fields the row actually reported are copied.
	kept := &statsSnapshot{GPU: map[string]gpuStat{"luid_c": {TemperatureC: 55, PowerWatts: 90}}}
	blank := map[string]gpuSensorSample{"luid_c": {TemperatureC: 57}}
	p.latest.Store(&blank)
	p.mergeInto(kept)
	if kept.GPU["luid_c"] != (gpuStat{TemperatureC: 57, PowerWatts: 90}) {
		t.Fatalf("luid_c = %+v, want the earlier wattage kept", kept.GPU["luid_c"])
	}
}

// TestLuidsByPCIAddressLive runs only when NVPAIR_LIVE_GPU=1: on a real host
// every nvidia-smi row must resolve to a distinct DXGI LUID through the
// D3DKMT adapter address, which is the join this file exists for. It also
// asserts that every card that reported a temperature reported a wattage —
// on a real NVIDIA adapter both come from the same driver query, so one
// arriving without the other means the new column was dropped somewhere
// between the query and the published map.
func TestLuidsByPCIAddressLive(t *testing.T) {
	if os.Getenv("NVPAIR_LIVE_GPU") == "" {
		t.Skip("set NVPAIR_LIVE_GPU=1 to run against the host's GPUs")
	}
	byAddr := luidsByPCIAddress()
	t.Logf("adapters by PCI address: %v", byAddr)
	sensors, err := nvidiaSmiSensors()
	if err != nil {
		t.Fatalf("nvidia-smi: %v", err)
	}
	t.Logf("nvidia-smi sensors: %v", sensors)
	seen := map[string]bool{}
	metered := 0
	for addr, sample := range sensors {
		luid, ok := byAddr[addr]
		if !ok {
			t.Errorf("nvidia-smi row %s (%+v) has no DXGI adapter with that PCI address", addr, sample)
			continue
		}
		if seen[luid] {
			t.Errorf("two nvidia-smi rows resolved to LUID %s", luid)
		}
		seen[luid] = true
		if sample.PowerWatts > 0 {
			metered++
		}
	}
	if metered == 0 {
		t.Error("no adapter reported power.draw; the new column never reached the parsed sample")
	}
	t.Logf("%d of %d adapters reported a wattage", metered, len(sensors))

	p := startGPUTempPoller()
	defer p.Stop()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if m := p.latest.Load(); m != nil && len(*m) == len(sensors) {
			t.Logf("poller published %v", *m)
			for luid, sample := range *m {
				if sample.PowerWatts <= 0 {
					t.Errorf("adapter %s published no wattage: %+v", luid, sample)
				}
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("poller never published %d adapter samples", len(sensors))
}
