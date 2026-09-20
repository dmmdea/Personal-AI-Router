// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"nvpair-shared/noderec"
)

// Live hardware check for the Windows Hailo path. It is skipped unless
// NVPAIR_LIVE_HAILO=1, so `go test ./...` on a developer machine never
// depends on a module being fitted; build it with `go test -c` and run the
// binary on a host that has one.
//
// Everything a unit test can assert about this path is asserted in
// accel_windows_test.go against injected readers. What cannot be faked is
// whether the real library, the real driver and the real firmware answer at
// all — a DLL binding that resolves the wrong export or a struct that is one
// field out both compile, pass, and then return garbage. That is what this
// test is for, so it reads the actual numbers and prints them.
func TestLiveHailoAccelerator(t *testing.T) {
	if os.Getenv("NVPAIR_LIVE_HAILO") != "1" {
		t.Skip("set NVPAIR_LIVE_HAILO=1 on a host with a Hailo module to run this")
	}
	slog.SetLogLoggerLevel(slog.LevelDebug)

	lib, err := hailoRuntime()
	if err != nil {
		t.Fatalf("HailoRT did not load: %v", err)
	}
	t.Logf("HailoRT loaded from %s", lib.path)

	ids, err := lib.scanDevices()
	if err != nil {
		t.Fatalf("hailo_scan_devices: %v", err)
	}
	t.Logf("hailo_scan_devices: %d device(s): %v", len(ids), ids)
	if len(ids) == 0 {
		t.Fatal("no Hailo device scanned")
	}

	rows := detectAccelerators()
	if len(rows) != 1 {
		t.Fatalf("detectAccelerators returned %d rows, want 1: %+v", len(rows), rows)
	}
	row := rows[0]
	t.Logf("accelerator row: name=%q kind=%q statsKey=%q", row.Name, row.Kind, row.statsKey)
	if !strings.Contains(row.Name, "Hailo-8L") {
		t.Errorf("row name = %q, want it to contain %q", row.Name, "Hailo-8L")
	}
	if row.Kind != noderec.GPUKindAccelerator {
		t.Errorf("row kind = %q, want %q", row.Kind, noderec.GPUKindAccelerator)
	}

	// One direct read first, so a failure here names the exact call and
	// status rather than timing out behind the sampler.
	dev, err := lib.open(ids[0])
	if err != nil {
		t.Fatalf("hailo_create_device_by_id(%q): %v", ids[0], err)
	}
	ts0, ts1, samples, err := dev.temperature()
	dev.close()
	if err != nil {
		t.Fatalf("hailo_get_chip_temperature: %v", err)
	}
	t.Logf("hailo_get_chip_temperature: ts0=%.2f C ts1=%.2f C sample_count=%d", ts0, ts1, samples)

	samplers := startHailoSamplers()
	if len(samplers) != 1 {
		t.Fatalf("startHailoSamplers returned %d samplers, want 1", len(samplers))
	}
	defer func() {
		for _, s := range samplers {
			s.Stop()
		}
	}()

	var stat gpuStat
	deadline := time.Now().Add(20 * time.Second)
	for {
		var ok bool
		if stat, ok = samplers[0].Latest(); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no sample published within 20s")
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Logf("sampler published: temperature_celsius=%d utilization_percent=%d",
		stat.TemperatureC, stat.UtilizationPct)
	if stat.TemperatureC <= 20 {
		t.Errorf("temperature = %d C, want a plausible die temperature above 20 C", stat.TemperatureC)
	}
	if stat.UtilizationPct != 0 {
		t.Errorf("utilization = %d, want 0: HailoRT exposes no busy counter on Windows", stat.UtilizationPct)
	}

	// The whole path end to end: the static row joined to the live sample the
	// way the HTTP handler does it.
	body := buildResponse([]GPUInfo{row}, nil, 0,
		statsSnapshot{GPU: map[string]gpuStat{samplers[0].key: stat}}, "", nil)
	t.Logf("/v1/node-info body: %s", body)

	var typed NodeInfoResponse
	if err := json.Unmarshal(body, &typed); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(typed.GPUs) != 1 {
		t.Fatalf("response rows = %d, want 1", len(typed.GPUs))
	}
	if typed.GPUs[0].TemperatureCelsius != stat.TemperatureC {
		t.Errorf("response temperature = %d, want %d", typed.GPUs[0].TemperatureCelsius, stat.TemperatureC)
	}
	if typed.TelemetryValid {
		t.Error("an accelerator sample validated GPU telemetry")
	}
}
