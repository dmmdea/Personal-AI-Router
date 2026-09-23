// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"
	"time"
)

// TestBuildResponseRowMarkers pins the per-row markers on the wire, on the
// bytes a client receives:
//
//   - utilization_unavailable says "no busy counter", which an absent
//     utilization_percent cannot (a measured 0 is absent too). A detector sets
//     it outright; a sampled row gets it until its sampler reports a reading.
//   - inference_ready appears only as an explicit false, so every row that
//     makes no claim keeps the historical shape.
//   - a unified row whose used figure is dedicated + shared publishes none
//     when only the dedicated half was sampled.
func TestBuildResponseRowMarkers(t *testing.T) {
	static := []GPUInfo{
		{Name: "Discrete card", VramBytes: 8 << 30, statsKey: "a"},
		{Name: "Accelerator with no counter", Kind: "npu", UtilizationUnavailable: true, InferenceReady: notInferenceReady(), statsKey: "b"},
		{Name: "Sampled SoC GPU, read", MemoryPool: "unified", InferenceReady: notInferenceReady(), utilizationNeedsSample: true, statsKey: "c"},
		{Name: "Sampled SoC GPU, unread", MemoryPool: "unified", utilizationNeedsSample: true, statsKey: "d"},
		{Name: "Sampled SoC GPU, no stat yet", MemoryPool: "unified", utilizationNeedsSample: true, statsKey: "e"},
		{Name: "Windows iGPU", VramBytes: 64 << 30, MemoryPool: "unified", usedIncludesShared: true, statsKey: "f"},
	}
	snap := statsSnapshot{
		GPU: map[string]gpuStat{
			"a": {UtilizationPct: 0, VRAMUsed: 1 << 30},
			"b": {TemperatureC: 47},
			"c": {UtilizationPct: 0, UtilizationKnown: true},
			"d": {TemperatureC: 40},
			"f": {UtilizationPct: 12, VRAMUsed: 8 << 20},
		},
		GPUSampledAt: time.Unix(1, 0),
	}
	_, raw := buildResponseDecode(t, static, nil, 0, snap)
	rows, _ := raw["GPUs"].([]any)
	if len(rows) != len(static) {
		t.Fatalf("GPUs = %v", raw["GPUs"])
	}
	row := func(i int) map[string]any {
		r, _ := rows[i].(map[string]any)
		return r
	}

	// An idle discrete card: no utilization key, and no marker either — the
	// client keeps reading that absence as 0 %, as it always has.
	if _, present := row(0)["utilization_unavailable"]; present {
		t.Errorf("idle discrete row carries utilization_unavailable: %v", row(0))
	}
	if _, present := row(0)["inference_ready"]; present {
		t.Errorf("discrete row carries inference_ready: %v", row(0))
	}
	if row(1)["utilization_unavailable"] != true || row(1)["inference_ready"] != false {
		t.Errorf("accelerator row = %v, want utilization_unavailable and inference_ready=false", row(1))
	}
	// A sampler that read an idle 0 is a measurement.
	if _, present := row(2)["utilization_unavailable"]; present {
		t.Errorf("read SoC row carries utilization_unavailable: %v", row(2))
	}
	if row(2)["inference_ready"] != false {
		t.Errorf("SoC row inference_ready = %v, want false", row(2)["inference_ready"])
	}
	// A sampler that could not read, and one with no stat at all, are not.
	for _, i := range []int{3, 4} {
		if row(i)["utilization_unavailable"] != true {
			t.Errorf("row %d = %v, want utilization_unavailable", i, row(i))
		}
	}
	// The Windows iGPU keeps its measured utilization and, with no Shared
	// Usage sample, drops the dedicated-only half of its used figure.
	if row(5)["utilization_percent"] != float64(12) {
		t.Errorf("iGPU utilization = %v, want 12", row(5)["utilization_percent"])
	}
	if used, present := row(5)["vram_used_bytes"]; present {
		t.Errorf("iGPU vram_used_bytes = %v, want absent without a Shared Usage sample", used)
	}
	if row(5)["vram_bytes"] != float64(64<<30) {
		t.Errorf("iGPU vram_bytes = %v, want the pool ceiling", row(5)["vram_bytes"])
	}
}

// TestNotInferenceReadyIsPerRow guards the helper against ever handing two rows
// one shared pointer.
func TestNotInferenceReadyIsPerRow(t *testing.T) {
	a, b := notInferenceReady(), notInferenceReady()
	if a == b || *a || *b {
		t.Fatalf("notInferenceReady = %p/%v, %p/%v; want two distinct false values", a, *a, b, *b)
	}
}
