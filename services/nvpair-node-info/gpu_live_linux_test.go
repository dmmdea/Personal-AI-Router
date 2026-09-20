// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"nvpair-shared/noderec"
)

// Live checks against real hardware. Both are gated on an environment
// variable, because everywhere else they would be no-ops — and a no-op that
// passes proves nothing. They deliberately exercise the wiring the unit tests
// cannot: the real sysfs layout, the real nvidia-smi, and the response a
// client actually receives. The fake-tree tests above cover the parsing.

// TestLiveIntelNodeInfo runs on a host with a discrete NVIDIA card and an
// Intel integrated GPU — the exact machine whose iGPU this service used to
// drop. It asserts both halves of the fix: the Intel row exists at all, and
// the NVIDIA card is still first, because the composition order is what
// consumers read as "the node's GPU".
func TestLiveIntelNodeInfo(t *testing.T) {
	if os.Getenv("NVPAIR_LIVE_INTEL") != "1" {
		t.Skip("set NVPAIR_LIVE_INTEL=1 to run against a host with an Intel GPU")
	}

	intel := detectIntelGPUs(drmClassDir)
	for _, row := range intel {
		t.Logf("Intel row: name=%q stats_key=%q vram_bytes=%d memory_pool=%q uma=%v",
			row.Name, row.statsKey, row.VramBytes, row.MemoryPool, row.usesSystemMemoryUsage)
	}
	if len(intel) == 0 {
		t.Fatal("detectIntelGPUs found nothing on a host that has an Intel GPU")
	}
	if !strings.Contains(intel[0].Name, "UHD Graphics 630") {
		t.Errorf("Intel row name = %q, want it to contain \"UHD Graphics 630\"", intel[0].Name)
	}
	if !strings.HasPrefix(intel[0].statsKey, intelStatsKeyPrefix) {
		t.Errorf("statsKey = %q, want the %q prefix", intel[0].statsKey, intelStatsKeyPrefix)
	}
	if intel[0].MemoryPool != noderec.GPUMemoryPoolUnified || intel[0].VramBytes == 0 {
		t.Errorf("integrated GPU not reported as a unified pool (memory_pool=%q vram_bytes=%d)",
			intel[0].MemoryPool, intel[0].VramBytes)
	}
	// The measured defect, asserted on the hardware that showed it: this row
	// must reach the wire with a shared-pool ceiling and NO used figure. The
	// host's RAM usage went here before, and the card read "VRAM 25.5 GB /
	// 66 GB" while the iGPU held a framebuffer.
	if intel[0].usesSystemMemoryUsage {
		t.Error("Intel row still borrows the host's memory usage")
	}
	// Assemble the body a client actually receives, off the real collector:
	// the flag lives in the inventory but the omission happens in response
	// assembly, so only this proves the wire shape.
	collector := startStatsCollector()
	t.Cleanup(collector.Stop)
	time.Sleep(1500 * time.Millisecond)
	liveBody := buildResponseAt(detectGPUs(), detectCPU(), detectMemoryTotal(), collector.Snapshot(), "", nil, time.Now())
	var live struct {
		GPUs []map[string]any `json:"GPUs"`
	}
	if err := json.Unmarshal(liveBody, &live); err != nil {
		t.Fatalf("unmarshal live response: %v", err)
	}
	t.Logf("live response GPUs: %s", liveBody)
	var sawIntel bool
	for _, row := range live.GPUs {
		name, _ := row["name"].(string)
		if !strings.Contains(name, "UHD Graphics") && !strings.Contains(name, "Intel") {
			continue
		}
		sawIntel = true
		if got := row["memory_pool"]; got != noderec.GPUMemoryPoolUnified {
			t.Errorf("%q: memory_pool = %v, want %q", name, got, noderec.GPUMemoryPoolUnified)
		}
		if _, present := row["vram_used_bytes"]; present {
			t.Errorf("%q: vram_used_bytes present (%v); an Intel iGPU row must omit it", name, row["vram_used_bytes"])
		}
	}
	if !sawIntel {
		t.Error("no Intel row in the assembled response")
	}
	// The ceiling, verified on the hardware rather than assumed: i915 has no
	// sysfs busy counter and an iGPU has no sensor, so both fields stay absent.
	if intel[0].UtilizationPercent != 0 || intel[0].TemperatureCelsius != 0 {
		t.Errorf("Intel row carries fabricated telemetry: util=%d temp=%d",
			intel[0].UtilizationPercent, intel[0].TemperatureCelsius)
	}

	gpus := detectGPUs()
	names := make([]string, len(gpus))
	for i, g := range gpus {
		names[i] = g.Name
	}
	t.Logf("detectGPUs order: %q", names)
	if len(gpus) < 2 {
		t.Fatalf("detectGPUs returned %d row(s), want the NVIDIA card AND the Intel iGPU: %q", len(gpus), names)
	}
	if !strings.Contains(gpus[0].Name, "NVIDIA") {
		t.Errorf("first row = %q, want the NVIDIA card first", gpus[0].Name)
	}
	found := false
	for _, g := range gpus[1:] {
		if strings.Contains(g.Name, "UHD Graphics 630") {
			found = true
		}
	}
	if !found {
		t.Errorf("no Intel row after the NVIDIA card: %q", names)
	}

	// And the same thing through the wire contract a client reads.
	body := buildResponse(gpus, nil, 0, statsSnapshot{}, "live-test", nil)
	t.Logf("response: %s", body)
	var resp NodeInfoResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(resp.GPUs) != len(gpus) {
		t.Errorf("response listed %d GPU(s), want %d", len(resp.GPUs), len(gpus))
	}
}

// TestLiveAMDGPUName runs on the Radeon host and proves the reworked name
// table renders on real hardware: the measured 0x15e7 APU, whose PCI database
// entry is the bare codename "Barcelo", must publish its graphics generation.
func TestLiveAMDGPUName(t *testing.T) {
	if os.Getenv("NVPAIR_LIVE_AMD") != "1" {
		t.Skip("set NVPAIR_LIVE_AMD=1 to run against a host with an AMD GPU")
	}

	cards := listAMDCards(drmClassDir)
	for _, c := range cards {
		arch, named := amdGCArchitecture(c.deviceDir)
		t.Logf("AMD card: %s device_id=%q driver_gc_arch=%q gc_named=%v unified=%v",
			c.card, c.deviceID, arch, named, c.unifiedPool)
	}
	gpus := detectAMDGPUs(drmClassDir)
	if len(gpus) == 0 {
		t.Fatal("detectAMDGPUs found nothing on a host that has an AMD GPU")
	}
	for _, row := range gpus {
		t.Logf("AMD row: name=%q stats_key=%q vram_bytes=%d", row.Name, row.statsKey, row.VramBytes)
	}

	name := gpus[0].Name
	if !strings.HasPrefix(name, "AMD Radeon") {
		t.Errorf("name = %q, want it to start with \"AMD Radeon\"", name)
	}
	if !strings.Contains(name, "GCN 5.1") {
		t.Errorf("name = %q, want it to name the graphics generation \"GCN 5.1\" (gfx90c)", name)
	}
	if !strings.Contains(name, "Barcelo") {
		t.Errorf("name = %q, want it to keep the codename", name)
	}
	// The whole point of dropping the old string: a compute-unit count is not
	// derivable from the device id, so it must not be published.
	for _, banned := range []string{"Vega 6", "Vega 7", "Vega 8"} {
		if strings.Contains(name, banned) {
			t.Errorf("name = %q still claims a compute-unit count (%q)", name, banned)
		}
	}
}
