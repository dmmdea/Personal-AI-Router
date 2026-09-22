// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package noderec

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestMaxGPUUtilization pins the scheduler-facing node busy figure: the max
// over GPU rows, with every non-GPU row excluded. Both the scanner daemon and
// the broker's manual-node path derive NodeTelemetry through this helper, so
// a regression here would let a saturated Edge TPU push a node's GPU pressure
// band up for LLM work the GPU could still take.
func TestMaxGPUUtilization(t *testing.T) {
	cases := []struct {
		name string
		gpus []GPUInfo
		want uint32
	}{
		{name: "no gpus", gpus: nil, want: 0},
		{name: "single gpu", gpus: []GPUInfo{{Name: "g", UtilizationPercent: 37}}, want: 37},
		{name: "max across gpus", gpus: []GPUInfo{{UtilizationPercent: 12}, {UtilizationPercent: 80}, {UtilizationPercent: 5}}, want: 80},
		{
			name: "accelerator excluded",
			gpus: []GPUInfo{{Name: "NVIDIA A2", UtilizationPercent: 20}, {Name: "Google Coral Edge TPU", Kind: GPUKindAccelerator, UtilizationPercent: 100}},
			want: 20,
		},
		{
			name: "accelerator only reads as idle",
			gpus: []GPUInfo{{Name: "Google Coral Edge TPU", Kind: GPUKindAccelerator, UtilizationPercent: 100}},
			want: 0,
		},
		{
			// The rule is "GPUs only", not "everything but the kinds that
			// existed when this was written": a kind added later must be
			// skipped without anyone remembering to come back here.
			name: "an unknown kind is excluded too",
			gpus: []GPUInfo{{Name: "NVIDIA A2", UtilizationPercent: 20}, {Name: "something new", Kind: "fpga", UtilizationPercent: 100}},
			want: 20,
		},
		{
			name: "non-GPU rows only read as idle",
			gpus: []GPUInfo{
				{Name: "something new", Kind: "fpga", TemperatureCelsius: 44},
				{Name: "Google Coral Edge TPU", Kind: GPUKindAccelerator, UtilizationPercent: 100},
			},
			want: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MaxGPUUtilization(tc.gpus); got != tc.want {
				t.Fatalf("MaxGPUUtilization = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestGPUInfoWireShape pins that the new fields are additive: a GPU row
// serializes exactly as before (no kind, no temperature), and an accelerator
// row carries both.
func TestGPUInfoWireShape(t *testing.T) {
	gpu, _ := json.Marshal(GPUInfo{Name: "NVIDIA A2", VramBytes: 1, UtilizationPercent: 3})
	if s := string(gpu); strings.Contains(s, "kind") || strings.Contains(s, "temperature") {
		t.Fatalf("GPU row grew unexpected fields: %s", s)
	}
	accel, _ := json.Marshal(GPUInfo{Name: "Google Coral Edge TPU", Kind: GPUKindAccelerator, UtilizationPercent: 30, TemperatureCelsius: 52})
	for _, want := range []string{`"kind":"npu"`, `"temperature_celsius":52`, `"utilization_percent":30`} {
		if !strings.Contains(string(accel), want) {
			t.Fatalf("accelerator row missing %s: %s", want, accel)
		}
	}
	if strings.Contains(string(accel), "vram") {
		t.Fatalf("accelerator row must not report VRAM: %s", accel)
	}
}

// TestGPUInfoMemoryPoolWireShape pins the unified-memory contract at the
// package every consumer decodes through.
//
// The field exists because a shared capacity and a dedicated one are not the
// same claim, and nothing on the wire used to distinguish them: an Intel iGPU
// published the host's 66 GB as "vram_bytes" beside the host's 25.5 GB as
// "vram_used_bytes", and clients rendered "VRAM 25.5 GB / 66 GB" for a GPU
// that had allocated a framebuffer. Two properties keep that from returning.
//
// First, additive: a discrete row must serialize exactly as it did before, so
// a consumer that has never heard of memory_pool is unaffected. Second,
// orthogonal: the marker says where the CAPACITY comes from and says nothing
// about usage, so a producer can mark a pool without being obliged to invent
// a figure for what is spent in it — which is the whole point.
func TestGPUInfoMemoryPoolWireShape(t *testing.T) {
	discrete, _ := json.Marshal(GPUInfo{
		Name: "NVIDIA GeForce RTX 5060 Ti", VramBytes: 17179869184,
		VramUsedBytes: 2147483648, UtilizationPercent: 41,
	})
	if strings.Contains(string(discrete), "memory_pool") {
		t.Fatalf("a discrete row must not carry memory_pool: %s", discrete)
	}

	// A shared pool with nothing measuring what the device holds: ceiling
	// present, used figure absent. This is the Intel iGPU / Mali / RKNPU shape.
	unmeasured, _ := json.Marshal(GPUInfo{
		Name:      "Intel UHD Graphics 630 (Coffee Lake, Gen 9.5)",
		VramBytes: 70866960384, MemoryPool: GPUMemoryPoolUnified,
	})
	if !strings.Contains(string(unmeasured), `"memory_pool":"unified"`) {
		t.Fatalf("unified row missing memory_pool: %s", unmeasured)
	}
	if strings.Contains(string(unmeasured), "vram_used_bytes") {
		t.Fatalf("a unified row with no measurement must omit vram_used_bytes: %s", unmeasured)
	}

	// A shared pool whose device DOES measure its own allocation: an AMD APU,
	// an Apple Silicon GPU, a DGX Spark. Both keys travel.
	measured, _ := json.Marshal(GPUInfo{
		Name:      "AMD Radeon Vega Graphics (Barcelo, GCN 5.1)",
		VramBytes: 17179869184, VramUsedBytes: 3221225472,
		MemoryPool: GPUMemoryPoolUnified,
	})
	for _, want := range []string{`"memory_pool":"unified"`, `"vram_used_bytes":3221225472`} {
		if !strings.Contains(string(measured), want) {
			t.Fatalf("measured unified row missing %s: %s", want, measured)
		}
	}

	// Round-trips: a relay that decodes and re-encodes (the scanner, the
	// broker) must not drop the marker, because a hop that loses it turns a
	// shared ceiling back into a "VRAM" figure at the far end.
	var back GPUInfo
	if err := json.Unmarshal(unmeasured, &back); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if back.MemoryPool != GPUMemoryPoolUnified {
		t.Fatalf("MemoryPool did not survive a round trip: %+v", back)
	}
	if back.VramUsedBytes != 0 {
		t.Fatalf("VramUsedBytes = %d, want 0 after decoding a row that omitted it", back.VramUsedBytes)
	}
}

// TestGPUMemoryPoolUnifiedValue pins the literal. It is a wire value shared by
// three Go services and a TypeScript client that compares against its own copy
// of the string, so renaming the constant must not quietly change what any of
// them send or match on.
func TestGPUMemoryPoolUnifiedValue(t *testing.T) {
	if GPUMemoryPoolUnified != "unified" {
		t.Fatalf("GPUMemoryPoolUnified = %q, want \"unified\"", GPUMemoryPoolUnified)
	}
}

// TestPowerWattsWireShape pins the power field on both records the node
// inventory carries.
//
// Power is additive in exactly the way temperature was: a row from a device
// nothing meters must serialize as it did before this field existed, because
// most of the inventory IS unmeterable — an integrated GPU, a Mali GPU, an
// RKNPU, an Edge TPU, a Hailo module and the board row have no power
// telemetry at all, and a literal 0 there would render as "drawing nothing".
// So the only rows that carry power_watts are the ones whose driver actually
// metered them.
func TestPowerWattsWireShape(t *testing.T) {
	unmetered, _ := json.Marshal(GPUInfo{Name: "Intel UHD Graphics 630", VramBytes: 1, MemoryPool: GPUMemoryPoolUnified})
	if strings.Contains(string(unmetered), "power_watts") {
		t.Fatalf("a row with no meter must not carry power_watts: %s", unmetered)
	}
	accel, _ := json.Marshal(GPUInfo{Name: "Google Coral Edge TPU", Kind: GPUKindAccelerator, TemperatureCelsius: 52})
	if strings.Contains(string(accel), "power_watts") {
		t.Fatalf("an Edge TPU reports no power: %s", accel)
	}

	metered, _ := json.Marshal(GPUInfo{Name: "NVIDIA GeForce RTX 5070 Ti", VramBytes: 1, TemperatureCelsius: 38, PowerWatts: 36})
	for _, want := range []string{`"temperature_celsius":38`, `"power_watts":36`} {
		if !strings.Contains(string(metered), want) {
			t.Fatalf("metered GPU row missing %s: %s", want, metered)
		}
	}

	// A number, not a string: the desktop reads it with the same numeric
	// decoder every other telemetry field goes through.
	if strings.Contains(string(metered), `"power_watts":"`) {
		t.Fatalf("power_watts must be a JSON number: %s", metered)
	}

	coldCPU, _ := json.Marshal(CPUInfo{Name: "CPU", Cores: 8, UtilizationPercent: 3})
	if strings.Contains(string(coldCPU), "power_watts") {
		t.Fatalf("a CPU with no readable energy counter must not carry power_watts: %s", coldCPU)
	}
	hotCPU, _ := json.Marshal(CPUInfo{Name: "CPU", Cores: 8, TemperatureCelsius: 55, PowerWatts: 140})
	if !strings.Contains(string(hotCPU), `"power_watts":140`) {
		t.Fatalf("metered CPU row missing power_watts: %s", hotCPU)
	}

	// Round-trips: the scanner and the broker decode and re-encode every
	// record, and a hop that drops the field would blank the figure at the
	// far end exactly as it would have for the pool marker.
	var backGPU GPUInfo
	if err := json.Unmarshal(metered, &backGPU); err != nil {
		t.Fatalf("decode GPU: %v", err)
	}
	if backGPU.PowerWatts != 36 {
		t.Fatalf("GPUInfo.PowerWatts = %v, want 36 after a round trip", backGPU.PowerWatts)
	}
	var backCPU CPUInfo
	if err := json.Unmarshal(hotCPU, &backCPU); err != nil {
		t.Fatalf("decode CPU: %v", err)
	}
	if backCPU.PowerWatts != 140 {
		t.Fatalf("CPUInfo.PowerWatts = %v, want 140 after a round trip", backCPU.PowerWatts)
	}
}

// TestGPUInfoUtilizationAndReadinessMarkers pins the two per-row markers the
// relays carry: utilization_unavailable is present only when set, and
// inference_ready is present only when a node said it (so absent keeps its
// historical "no claim" meaning, and an explicit false survives the trip).
func TestGPUInfoUtilizationAndReadinessMarkers(t *testing.T) {
	const body = `[{"name":"Hailo-8L AI Accelerator","kind":"npu","utilization_unavailable":true,"inference_ready":false},` +
		`{"name":"NVIDIA GeForce RTX 5060","vram_bytes":8279556096}]`
	var rows []GPUInfo
	if err := json.Unmarshal([]byte(body), &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !rows[0].UtilizationUnavailable || rows[0].InferenceReady == nil || *rows[0].InferenceReady {
		t.Fatalf("accelerator row = %+v, want utilization_unavailable and inference_ready=false", rows[0])
	}
	if rows[1].UtilizationUnavailable || rows[1].InferenceReady != nil {
		t.Fatalf("GPU row = %+v, want neither marker", rows[1])
	}
	out, err := json.Marshal(rows)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if strings.Count(string(out), `"utilization_unavailable":true`) != 1 ||
		strings.Count(string(out), `"inference_ready":false`) != 1 {
		t.Fatalf("re-marshal lost or grew a marker: %s", out)
	}
}
