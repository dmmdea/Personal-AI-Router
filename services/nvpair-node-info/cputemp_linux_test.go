// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// writeHwmon lays out a fake /sys/class/hwmon tree: one directory per driver
// with temp<N>_input / temp<N>_label pairs.
func writeHwmon(t *testing.T, root string, drivers map[string][][2]string) {
	t.Helper()
	i := 0
	for name, sensors := range drivers {
		dir := filepath.Join(root, "hwmon"+string(rune('0'+i)))
		i++
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "name"), []byte(name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		for n, s := range sensors {
			base := filepath.Join(dir, "temp"+string(rune('1'+n)))
			if err := os.WriteFile(base+"_input", []byte(s[1]+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if s[0] != "" {
				if err := os.WriteFile(base+"_label", []byte(s[0]+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}

// TestFindHwmonCPUPackage pins the sensor choice: the coretemp "Package id 0"
// input wins over per-core inputs and over non-CPU drivers (nvme, acpitz),
// whatever the directory order.
func TestFindHwmonCPUPackage(t *testing.T) {
	root := t.TempDir()
	writeHwmon(t, root, map[string][][2]string{
		"acpitz":   {{"", "27800"}},
		"nvme":     {{"Composite", "41850"}},
		"coretemp": {{"Core 0", "82000"}, {"Package id 0", "80000"}, {"Core 1", "75000"}},
	})
	p := findHwmonCPUPackage(root)
	if filepath.Base(filepath.Dir(p)) == "" || filepath.Base(p) != "temp2_input" {
		t.Fatalf("picked %q, want the Package id 0 input (temp2_input)", p)
	}
	got, ok := (cpuTempSource{path: p}).read()
	if !ok || got != 80 {
		t.Fatalf("read = (%d, %v), want (80, true)", got, ok)
	}
}

func TestFindHwmonCPUPackageAMDAndFallbacks(t *testing.T) {
	root := t.TempDir()
	writeHwmon(t, root, map[string][][2]string{
		"k10temp": {{"Tctl", "61125"}, {"Tdie", "58000"}},
	})
	if p := findHwmonCPUPackage(root); filepath.Base(p) != "temp1_input" {
		t.Fatalf("k10temp: picked %q, want Tctl (temp1_input)", p)
	}

	unlabelled := t.TempDir()
	writeHwmon(t, unlabelled, map[string][][2]string{
		"cpu_thermal": {{"", "45000"}},
	})
	if p := findHwmonCPUPackage(unlabelled); filepath.Base(p) != "temp1_input" {
		t.Fatalf("unlabelled CPU driver: picked %q, want its first input", p)
	}

	none := t.TempDir()
	writeHwmon(t, none, map[string][][2]string{
		"nvme": {{"Composite", "41850"}},
	})
	if p := findHwmonCPUPackage(none); p != "" {
		t.Fatalf("no CPU driver: picked %q, want none", p)
	}
	if p := findHwmonCPUPackage(filepath.Join(none, "missing")); p != "" {
		t.Fatalf("missing root: picked %q, want none", p)
	}
}

func TestFindThermalZone(t *testing.T) {
	root := t.TempDir()
	for i, typ := range []string{"acpitz", "x86_pkg_temp"} {
		dir := filepath.Join(root, "thermal_zone"+string(rune('0'+i)))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "type"), []byte(typ+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "temp"), []byte("83000\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p := findThermalZone(root, "x86_pkg_temp")
	if filepath.Base(filepath.Dir(p)) != "thermal_zone1" {
		t.Fatalf("picked %q, want thermal_zone1/temp", p)
	}
	if got, ok := (cpuTempSource{path: p}).read(); !ok || got != 83 {
		t.Fatalf("read = (%d, %v), want (83, true)", got, ok)
	}
	if p := findThermalZone(root, "nope"); p != "" {
		t.Fatalf("unknown zone type picked %q", p)
	}
}

func TestCPUTempSourceEmpty(t *testing.T) {
	if got, ok := (cpuTempSource{}).read(); ok || got != 0 {
		t.Fatalf("empty source read = (%d, %v), want (0, false)", got, ok)
	}
}

// TestParseNvidiaDynamicTemperature pins the fourth column: a numeric
// temperature lands on the stat, [N/A] and a missing column leave it zero,
// and the utilization sample count is unaffected either way.
func TestParseNvidiaDynamicTemperature(t *testing.T) {
	res, samples := parseNvidiaDynamic("GPU-a, 100, 14686, 85\nGPU-b, 3, 10, [N/A]\nGPU-c, 7, 20\n")
	if samples != 3 {
		t.Fatalf("utilization samples = %d, want 3", samples)
	}
	if res["GPU-a"].TemperatureC != 85 || res["GPU-a"].UtilizationPct != 100 {
		t.Fatalf("GPU-a = %+v", res["GPU-a"])
	}
	if res["GPU-b"].TemperatureC != 0 || res["GPU-c"].TemperatureC != 0 {
		t.Fatalf("N/A or missing temperature must stay zero: b=%+v c=%+v", res["GPU-b"], res["GPU-c"])
	}
}
