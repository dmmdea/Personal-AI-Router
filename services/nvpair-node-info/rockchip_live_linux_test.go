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

// TestLiveRockchipNodeInfo runs the real detectors and the real collector on a
// Rockchip board and asserts the inventory a client would receive. It is gated
// on NVPAIR_LIVE_ROCKCHIP=1 because it reads this host's sysfs: everywhere else
// it would be a no-op, and a no-op that passes proves nothing.
//
// It deliberately exercises the wiring rather than the parsers: detection ->
// sampler goroutines -> the collector's 1 s tick -> the marshaled response. The
// unit tests above cover the parsing against fake trees.
func TestLiveRockchipNodeInfo(t *testing.T) {
	if os.Getenv("NVPAIR_LIVE_ROCKCHIP") != "1" {
		t.Skip("set NVPAIR_LIVE_ROCKCHIP=1 to run against a Rockchip board")
	}
	t.Cleanup(stopRockchipSamplers)

	rows := detectRockchipDevices()
	if len(rows) != 2 {
		t.Fatalf("detectRockchipDevices returned %d row(s), want the Mali GPU and the NPU: %+v", len(rows), rows)
	}

	var gpu, npu GPUInfo
	for _, row := range rows {
		switch row.Kind {
		case noderec.GPUKindAccelerator:
			npu = row
		default:
			gpu = row
		}
	}
	t.Logf("GPU row: name=%q stats_key=%q vram_bytes=%d memory_pool=%q", gpu.Name, gpu.statsKey, gpu.VramBytes, gpu.MemoryPool)
	t.Logf("NPU row: name=%q kind=%q stats_key=%q vram_bytes=%d memory_pool=%q", npu.Name, npu.Kind, npu.statsKey, npu.VramBytes, npu.MemoryPool)

	if !strings.Contains(gpu.Name, "Mali-G610") {
		t.Errorf("GPU name = %q, want it to contain Mali-G610", gpu.Name)
	}
	if npu.Kind != noderec.GPUKindAccelerator {
		t.Errorf("NPU kind = %q, want %q", npu.Kind, noderec.GPUKindAccelerator)
	}
	if !strings.Contains(npu.Name, "RK3588") {
		t.Errorf("NPU name = %q, want it to contain RK3588", npu.Name)
	}
	for _, row := range []GPUInfo{gpu, npu} {
		if row.MemoryPool != noderec.GPUMemoryPoolUnified || row.VramBytes == 0 {
			t.Errorf("%q: unified pool not reported (memory_pool=%q vram_bytes=%d)", row.Name, row.MemoryPool, row.VramBytes)
		}
		if row.usesSystemMemoryUsage {
			t.Errorf("%q: still borrows the board's RAM usage as its own", row.Name)
		}
	}

	cpu := detectCPU()
	if cpu == nil {
		t.Fatal("detectCPU returned nil on the board")
	}
	t.Logf("CPU: name=%q cores=%d", cpu.Name, cpu.Cores)
	if !strings.Contains(cpu.Name, "Orange Pi 5") || !strings.Contains(cpu.Name, "RK3588S") {
		t.Errorf("CPU name = %q, want it to name both the SoC (RK3588S) and the board (Orange Pi 5)", cpu.Name)
	}
	if cpu.Cores != 8 {
		t.Errorf("CPU cores = %d, want 8 (4x Cortex-A76 + 4x Cortex-A55)", cpu.Cores)
	}

	// The real collector: its tick is what folds the sampler output into the
	// snapshot every HTTP response is built from.
	collector := startStatsCollector()
	t.Cleanup(collector.Stop)
	deadline := time.Now().Add(6 * time.Second)
	var snap statsSnapshot
	for {
		snap = collector.Snapshot()
		_, haveGPU := snap.GPU[gpu.statsKey]
		_, haveNPU := snap.GPU[npu.statsKey]
		if haveGPU && haveNPU {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("collector published no sample for %q / %q within the deadline: %+v", gpu.statsKey, npu.statsKey, snap.GPU)
		}
		time.Sleep(250 * time.Millisecond)
	}

	gpuStats, npuStats := snap.GPU[gpu.statsKey], snap.GPU[npu.statsKey]
	t.Logf("GPU sample: utilization=%d%% temperature=%dC", gpuStats.UtilizationPct, gpuStats.TemperatureC)
	t.Logf("NPU sample: utilization=%d%% temperature=%dC", npuStats.UtilizationPct, npuStats.TemperatureC)
	t.Logf("CPU sample: utilization=%d%% temperature=%dC, memory used=%d bytes", snap.CPUUtilPct, snap.CPUTempC, snap.MemUsedBytes)
	if gpuStats.TemperatureC == 0 {
		t.Error("GPU temperature is 0; the gpu-thermal zone was not read")
	}
	if npuStats.TemperatureC == 0 {
		t.Error("NPU temperature is 0; the npu-thermal zone was not read")
	}
	if snap.CPUTempC == 0 {
		t.Error("CPU temperature is 0; no thermal zone matched on a board that publishes soc-thermal")
	}
	if snap.MemUsedBytes == 0 {
		t.Error("system memory used is 0; the node's memory object would report nothing")
	}

	body := buildResponseAt(rows, cpu, detectMemoryTotal(), snap, "", nil, time.Now())
	t.Logf("/v1/node-info body: %s", body)

	// The wire shape, on the board that showed the defect: both rows carry the
	// shared-pool marker and neither carries a used figure. snap.MemUsedBytes
	// is non-zero above, so a row that still mapped it would be visible here.
	var live struct {
		GPUs []map[string]any `json:"GPUs"`
	}
	if err := json.Unmarshal(body, &live); err != nil {
		t.Fatalf("unmarshal live response: %v", err)
	}
	if len(live.GPUs) != len(rows) {
		t.Fatalf("response carries %d rows, want %d", len(live.GPUs), len(rows))
	}
	for _, row := range live.GPUs {
		name, _ := row["name"].(string)
		if got := row["memory_pool"]; got != noderec.GPUMemoryPoolUnified {
			t.Errorf("%q: memory_pool = %v, want %q", name, got, noderec.GPUMemoryPoolUnified)
		}
		if _, present := row["vram_used_bytes"]; present {
			t.Errorf("%q: vram_used_bytes present (%v); nothing on this SoC measures it", name, row["vram_used_bytes"])
		}
	}
}
