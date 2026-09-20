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
