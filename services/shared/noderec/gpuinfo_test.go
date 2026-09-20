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
			name: "board row excluded",
			gpus: []GPUInfo{{Name: "NVIDIA A2", UtilizationPercent: 20}, {Name: "ROG Dual Intelligent Processors", Kind: GPUKindBoard, TemperatureCelsius: 44}},
			want: 20,
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
				{Name: "ROG Dual Intelligent Processors", Kind: GPUKindBoard, TemperatureCelsius: 44},
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
	board, _ := json.Marshal(GPUInfo{Name: "ROG Dual Intelligent Processors", Kind: GPUKindBoard, TemperatureCelsius: 44})
	for _, want := range []string{`"kind":"board"`, `"temperature_celsius":44`} {
		if !strings.Contains(string(board), want) {
			t.Fatalf("board row missing %s: %s", want, board)
		}
	}
	// A board controller has no VRAM and publishes no utilization; both must
	// drop out rather than appear as a zero a client would render.
	for _, absent := range []string{"vram", "utilization"} {
		if strings.Contains(string(board), absent) {
			t.Fatalf("board row carries %s: %s", absent, board)
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
