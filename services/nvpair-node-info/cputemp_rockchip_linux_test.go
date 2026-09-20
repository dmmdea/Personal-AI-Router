// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"path/filepath"
	"testing"
)

// TestFindCPUTempSourceZoneOrder pins the thermal-zone chain used when no hwmon
// driver names a CPU sensor — the state on an Arm SoC, whose hwmon entries are
// named after thermal zones (soc_thermal, bigcore0_thermal, ...) and match no
// CPU driver. The first zone type present in cpuThermalZoneTypes order wins,
// and the per-cluster zones are never picked.
func TestFindCPUTempSourceZoneOrder(t *testing.T) {
	cases := []struct {
		name     string
		zones    []string
		wantZone string // thermal_zone<N> directory expected, "" for no source
	}{
		{
			name:     "x86 package zone outranks everything",
			zones:    []string{"acpitz", "cpu-thermal", "x86_pkg_temp"},
			wantZone: "thermal_zone2",
		},
		{
			name:     "cpu-thermal outranks soc-thermal",
			zones:    []string{"soc-thermal", "cpu-thermal"},
			wantZone: "thermal_zone1",
		},
		{
			name:     "measured RK3588S zone set: soc-thermal, never a cluster zone",
			zones:    []string{"soc-thermal", "bigcore0-thermal", "bigcore1-thermal", "littlecore-thermal", "center-thermal", "gpu-thermal", "npu-thermal"},
			wantZone: "thermal_zone0",
		},
		{
			name:     "underscored spelling is accepted last",
			zones:    []string{"gpu-thermal", "cpu_thermal"},
			wantZone: "thermal_zone1",
		},
		{
			name:  "no CPU zone at all",
			zones: []string{"gpu-thermal", "npu-thermal"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hwmon, thermal := t.TempDir(), t.TempDir()
			for i, zoneType := range tc.zones {
				writeThermalZone(t, thermal, i, zoneType, "48300")
			}
			src := findCPUTempSourceIn(hwmon, thermal)
			if tc.wantZone == "" {
				if src.path != "" {
					t.Fatalf("picked %q, want no source", src.path)
				}
				return
			}
			if filepath.Base(filepath.Dir(src.path)) != tc.wantZone {
				t.Fatalf("picked %q, want %s/temp", src.path, tc.wantZone)
			}
			if got, ok := src.read(); !ok || got != 48 {
				t.Fatalf("read = (%d, %v), want (48, true)", got, ok)
			}
		})
	}
}

// TestFindCPUTempSourceHwmonWins pins that a named hwmon CPU driver still takes
// precedence over any thermal zone.
func TestFindCPUTempSourceHwmonWins(t *testing.T) {
	hwmon, thermal := t.TempDir(), t.TempDir()
	writeHwmon(t, hwmon, map[string][][2]string{
		"coretemp": {{"Package id 0", "80000"}},
	})
	writeThermalZone(t, thermal, 0, "x86_pkg_temp", "48300")
	src := findCPUTempSourceIn(hwmon, thermal)
	if filepath.Base(src.path) != "temp1_input" {
		t.Fatalf("picked %q, want the hwmon package input", src.path)
	}
	if got, ok := src.read(); !ok || got != 80 {
		t.Fatalf("read = (%d, %v), want (80, true)", got, ok)
	}
}
