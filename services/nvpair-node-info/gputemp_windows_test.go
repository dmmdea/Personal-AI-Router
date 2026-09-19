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

func TestParseNvidiaTemperatures(t *testing.T) {
	got := parseNvidiaTemperatures("00000000:17:00.0, 44\n00000000:65:00.0, 43\n00000000:B5:00.0, [N/A]\nbroken row\n")
	want := map[string]uint32{"17:00.0": 44, "65:00.0": 43}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseNvidiaTemperatures = %v, want %v", got, want)
	}
}

// TestGPUTempPollerMergeInto pins the merge: temperatures land on the LUID
// keys the PDH decoders use, existing VRAM/utilization survive, a snapshot
// map that may alias a published one is cloned rather than mutated, and a
// poller with nothing yet is a no-op.
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

	temps := map[string]uint32{"luid_a": 61, "luid_b": 40}
	p.latest.Store(&temps)
	p.mergeInto(snap)
	if snap.GPU["luid_a"] != (gpuStat{VRAMUsed: 5, UtilizationPct: 40, TemperatureC: 61}) {
		t.Fatalf("luid_a = %+v", snap.GPU["luid_a"])
	}
	if snap.GPU["luid_b"] != (gpuStat{TemperatureC: 40}) {
		t.Fatalf("luid_b = %+v", snap.GPU["luid_b"])
	}
	if _, mutated := published["luid_b"]; mutated || published["luid_a"].TemperatureC != 0 {
		t.Fatalf("published map was mutated: %v", published)
	}
}

// TestLuidsByPCIAddressLive runs only when NVPAIR_LIVE_GPU=1: on a real host
// every nvidia-smi row must resolve to a distinct DXGI LUID through the
// D3DKMT adapter address, which is the join this file exists for.
func TestLuidsByPCIAddressLive(t *testing.T) {
	if os.Getenv("NVPAIR_LIVE_GPU") == "" {
		t.Skip("set NVPAIR_LIVE_GPU=1 to run against the host's GPUs")
	}
	byAddr := luidsByPCIAddress()
	t.Logf("adapters by PCI address: %v", byAddr)
	temps, err := nvidiaSmiTemperatures()
	if err != nil {
		t.Fatalf("nvidia-smi: %v", err)
	}
	t.Logf("nvidia-smi temperatures: %v", temps)
	seen := map[string]bool{}
	for addr, c := range temps {
		luid, ok := byAddr[addr]
		if !ok {
			t.Errorf("nvidia-smi row %s (%d C) has no DXGI adapter with that PCI address", addr, c)
			continue
		}
		if seen[luid] {
			t.Errorf("two nvidia-smi rows resolved to LUID %s", luid)
		}
		seen[luid] = true
	}
	p := startGPUTempPoller()
	defer p.Stop()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if m := p.latest.Load(); m != nil && len(*m) == len(temps) {
			t.Logf("poller published %v", *m)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("poller never published %d temperatures", len(temps))
}
