// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"testing"
	"time"

	"nvpair-shared/noderec"
)

// TestBuildResponseAcceleratorRow pins the accelerator path through response
// assembly on every platform: the row keeps its kind, picks up the sampler's
// utilization and temperature by statsKey, reports no VRAM, and does not by
// itself make the node's GPU telemetry valid.
func TestBuildResponseAcceleratorRow(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	body := buildResponseAt(
		[]GPUInfo{
			{Name: "NVIDIA A2", VramBytes: 16 << 30, statsKey: "GPU-uuid"},
			{Name: "Google Coral Edge TPU", Kind: noderec.GPUKindAccelerator, statsKey: "apex:apex_0"},
		},
		nil,
		0,
		statsSnapshot{
			GPU: map[string]gpuStat{
				"GPU-uuid":    {UtilizationPct: 12, VRAMUsed: 1 << 30},
				"apex:apex_0": {UtilizationPct: 30, TemperatureC: 52},
			},
			// No GPUSampledAt: the accelerator sample alone must not validate telemetry.
		},
		"",
		nil,
		now,
	)

	var typed NodeInfoResponse
	if err := json.Unmarshal(body, &typed); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if typed.TelemetryValid {
		t.Fatal("accelerator sample validated GPU telemetry")
	}
	if len(typed.GPUs) != 2 {
		t.Fatalf("GPUs = %d rows, want 2", len(typed.GPUs))
	}
	gpu, accel := typed.GPUs[0], typed.GPUs[1]
	if gpu.Kind != "" || gpu.TemperatureCelsius != 0 || gpu.UtilizationPercent != 12 {
		t.Fatalf("GPU row = %+v", gpu)
	}
	if accel.Kind != noderec.GPUKindAccelerator || accel.UtilizationPercent != 30 || accel.TemperatureCelsius != 52 {
		t.Fatalf("accelerator row = %+v", accel)
	}
	if accel.VramBytes != 0 || accel.VramUsedBytes != 0 {
		t.Fatalf("accelerator row reports VRAM: %+v", accel)
	}

	var raw struct {
		GPUs []map[string]any `json:"GPUs"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	if _, ok := raw.GPUs[0]["kind"]; ok {
		t.Fatal("GPU row must omit kind")
	}
	if raw.GPUs[1]["kind"] != "npu" || raw.GPUs[1]["temperature_celsius"] != float64(52) {
		t.Fatalf("accelerator wire row = %v", raw.GPUs[1])
	}
	if _, ok := raw.GPUs[1]["vram_bytes"]; ok {
		t.Fatal("accelerator row must omit vram_bytes")
	}
}
