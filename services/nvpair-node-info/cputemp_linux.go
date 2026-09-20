// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Linux CPU package temperature, read per collector tick from sysfs — no
// daemon, no cgo.
//
// The kernel's hwmon class exposes one directory per sensor driver. The CPU
// package sensor is the driver named coretemp (Intel), k10temp / zenpower
// (AMD) or cpu_thermal (many ARM SoCs). Each has temp<N>_input files in
// millidegrees Celsius, optionally labelled by temp<N>_label; the package
// reading is the one labelled "Package id 0" (coretemp) or "Tctl"/"Tdie"
// (k10temp). When no label matches, the first input of the CPU driver is used.
// When no hwmon driver matches at all — an Arm SoC names its hwmon entries
// after thermal zones (soc_thermal, bigcore0_thermal, ...) rather than after a
// CPU driver — the thermal class is searched instead, in cpuThermalZoneTypes
// order.
//
// A host that has none of these (a VM, an unusual board) reports nothing, and
// the omitempty tag drops cpu.temperature_celsius from the JSON.

const (
	hwmonClassDir   = "/sys/class/hwmon"
	thermalClassDir = "/sys/class/thermal"
)

// cpuHwmonDrivers are the hwmon `name` values that identify a CPU sensor.
var cpuHwmonDrivers = map[string]bool{
	"coretemp":    true,
	"k10temp":     true,
	"zenpower":    true,
	"cpu_thermal": true,
}

// cpuPackageLabels are the temp<N>_label values that identify the package (as
// opposed to per-core) reading, in preference order.
var cpuPackageLabels = []string{"package id 0", "tctl", "tdie", "cpu"}

// cpuThermalZoneTypes are the thermal-zone `type` values read when no hwmon CPU
// driver matched, in preference order: the x86 package zone first, then the
// zone names Arm SoCs use. A board with per-cluster zones (RK3588S publishes
// bigcore0/bigcore1/littlecore alongside soc-thermal) has no single "the CPU"
// sensor, so the SoC-wide zone is the honest answer and is preferred over
// arbitrarily picking one cluster.
var cpuThermalZoneTypes = []string{"x86_pkg_temp", "cpu-thermal", "soc-thermal", "cpu_thermal"}

// hwmonTempSensor is one temp<N>_input of a CPU hwmon driver.
type hwmonTempSensor struct {
	input string // path to temp<N>_input
	label string // lowercase temp<N>_label contents, "" when absent
}

// cpuTempSource resolves once (at collector start) which sysfs file holds the
// package temperature, so the per-tick read is a single file read.
type cpuTempSource struct {
	path string
}

// findCPUTempSource locates the package sensor. Returns an empty source when
// the host exposes none.
func findCPUTempSource() cpuTempSource {
	return findCPUTempSourceIn(hwmonClassDir, thermalClassDir)
}

// findCPUTempSourceIn is findCPUTempSource against explicit class roots: hwmon
// first (a named CPU driver is unambiguous), then the thermal zones in
// cpuThermalZoneTypes order, first present wins. Parameterized so the search
// order is testable against a fake tree.
func findCPUTempSourceIn(hwmonRoot, thermalRoot string) cpuTempSource {
	if p := findHwmonCPUPackage(hwmonRoot); p != "" {
		return cpuTempSource{path: p}
	}
	for _, zoneType := range cpuThermalZoneTypes {
		if p := findThermalZone(thermalRoot, zoneType); p != "" {
			return cpuTempSource{path: p}
		}
	}
	return cpuTempSource{}
}

// findHwmonCPUPackage walks <root>/hwmon*/ for a CPU driver and picks its
// package input by label, falling back to its lowest-numbered input.
func findHwmonCPUPackage(root string) string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		dir := filepath.Join(root, name)
		driver := strings.TrimSpace(readSysfs(filepath.Join(dir, "name")))
		if !cpuHwmonDrivers[driver] {
			continue
		}
		if p := pickPackageSensor(listHwmonTempSensors(dir)); p != "" {
			return p
		}
	}
	return ""
}

// listHwmonTempSensors returns the temp<N>_input files of one hwmon
// directory with their labels, sorted by file name.
func listHwmonTempSensors(dir string) []hwmonTempSensor {
	inputs, _ := filepath.Glob(filepath.Join(dir, "temp*_input"))
	sort.Strings(inputs)
	var out []hwmonTempSensor
	for _, in := range inputs {
		labelPath := strings.TrimSuffix(in, "_input") + "_label"
		out = append(out, hwmonTempSensor{
			input: in,
			label: strings.ToLower(strings.TrimSpace(readSysfs(labelPath))),
		})
	}
	return out
}

// pickPackageSensor chooses the package reading: the first sensor whose label
// matches cpuPackageLabels in preference order, else the first sensor.
func pickPackageSensor(sensors []hwmonTempSensor) string {
	for _, want := range cpuPackageLabels {
		for _, s := range sensors {
			if s.label == want {
				return s.input
			}
		}
	}
	if len(sensors) > 0 {
		return sensors[0].input
	}
	return ""
}

// findThermalZone returns <root>/thermal_zone*/temp for the zone whose type
// matches, or "".
func findThermalZone(root, zoneType string) string {
	zones, _ := filepath.Glob(filepath.Join(root, "thermal_zone*"))
	sort.Strings(zones)
	for _, z := range zones {
		if strings.TrimSpace(readSysfs(filepath.Join(z, "type"))) == zoneType {
			return filepath.Join(z, "temp")
		}
	}
	return ""
}

// read returns the package temperature in whole degrees, or false when the
// source is absent or unreadable this tick.
func (s cpuTempSource) read() (uint32, bool) {
	if s.path == "" {
		return 0, false
	}
	return parseMillidegrees(readSysfs(s.path))
}
