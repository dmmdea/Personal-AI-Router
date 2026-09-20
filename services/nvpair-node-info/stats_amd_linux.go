// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Per-tick amdgpu sampling, the dynamic half of the AMD support whose
// inventory half lives in gpu_amd_linux.go.
//
// Every figure is one small sysfs read, so this runs inline on the collector's
// 1 s tick rather than in a sampler goroutine of its own (the accelerator
// sampler in accel_linux.go needs a goroutine because its utilization is a
// derivative of an interrupt counter observed at 100 ms; amdgpu keeps the busy
// percentage itself).
//
// The three numbers:
//
//   - utilization: gpu_busy_percent, already the 0..100 figure nvidia-smi's
//     utilization.gpu reports. A value outside that range, or an unreadable
//     file, is dropped rather than published as 0 - a driver that cannot say
//     must not look idle.
//   - VRAM used: mem_info_vram_used, plus mem_info_gtt_used on a unified part,
//     matching the capacity rule so used never exceeds total.
//   - temperature: the amdgpu hwmon's edge sensor (temp*_input, millidegrees).
//   - power: the same hwmon's power1_input, the average socket power the SMU
//     reports (labelled PPT — package power tracking). It is in microwatts,
//     and on an APU it covers the whole package, CPU cores included, which is
//     what "the GPU is drawing" means on a part with no separate rail.
//
// A host without amdgpu costs one os.ReadDir of /sys/class/drm per tick and
// logs nothing at all; a host without /sys/class/drm logs a single Debug line
// for the process lifetime (listAMDCards).

// amdHwmonName is the hwmon driver name amdgpu registers its sensors under.
// Sensors from a differently named hwmon on the same device are still usable,
// they are just considered after amdgpu's own.
const amdHwmonName = "amdgpu"

// amdEdgeLabel is the temp*_label value amdgpu gives the GPU edge sensor, the
// closest analogue to nvidia-smi's temperature.gpu. The same hwmon may also
// expose "junction" (hotspot) and "mem", which read higher and would make an
// AMD row look hot next to an NVIDIA one.
const amdEdgeLabel = "edge"

// amdPPTLabel is the power1_label value amdgpu gives its average socket power
// input. The same hwmon may expose power2 ("PPT" instantaneous on some SKUs)
// or none at all; an input labelled anything else is not the figure this row
// wants, so the unlabelled fallback is power1 and nothing further.
const amdPPTLabel = "ppt"

// amdPowerInput is the hwmon attribute holding that reading, in microwatts,
// and amdPowerLabel is the attribute naming it.
const (
	amdPowerInput = "power1_input"
	amdPowerLabel = "power1_label"
)

// amdSampleAt folds one amdgpu sampling pass into the tick's GPU map and
// reports the sample time the snapshot should carry. A usable utilization
// reading is fresh GPU telemetry exactly as an nvidia-smi one is, so it
// advances sampledAt (and with it TelemetryValid); anything less leaves the
// caller's value untouched. This is the single line stats_linux.go adds.
func amdSampleAt(out map[string]gpuStat, sampledAt time.Time) time.Time {
	if decodeAMD(drmClassDir, out) {
		return time.Now()
	}
	return sampledAt
}

// decodeAMD samples every AMD card under drmRoot into out, keyed by the same
// statsKey the inventory stamped on the matching GPUInfo. sampled reports
// whether any card produced a valid utilization reading - the condition that
// marks the snapshot freshly sampled. A card that yields no figure at all is
// left out of the map entirely, so a transient sysfs failure keeps the row's
// previous values rather than publishing zeros over them.
func decodeAMD(drmRoot string, out map[string]gpuStat) bool {
	sampled := false
	for _, c := range listAMDCards(drmRoot) {
		var stat gpuStat
		any := false
		if pct, ok := amdBusyPercent(c.deviceDir); ok {
			stat.UtilizationPct = pct
			sampled = true
			any = true
		}
		if used, ok := amdUsedBytes(c); ok {
			stat.VRAMUsed = used
			any = true
		}
		if temp, ok := amdEdgeTempC(c.deviceDir); ok {
			stat.TemperatureC = temp
			any = true
		}
		if watts, ok := amdPowerWatts(c.deviceDir); ok {
			stat.PowerWatts = watts
			any = true
		}
		if any {
			out[c.statsKey] = stat
		}
	}
	return sampled
}

// amdBusyPercent reads gpu_busy_percent. Out-of-range values are rejected:
// the attribute is documented as 0..100 and anything else means we are not
// reading what we think we are.
func amdBusyPercent(deviceDir string) (uint32, bool) {
	pct, ok := drmSysfsUint(deviceDir, "gpu_busy_percent")
	if !ok || pct > 100 {
		return 0, false
	}
	return uint32(pct), true
}

// amdUsedBytes is the card's in-use memory: driver-reported VRAM, plus the
// GTT the driver has mapped when the row's capacity included the GTT
// aperture. ok is false when the driver reports no VRAM usage at all.
func amdUsedBytes(c amdCard) (uint64, bool) {
	used, ok := drmSysfsUint(c.deviceDir, "mem_info_vram_used")
	if !ok {
		return 0, false
	}
	if c.unifiedPool {
		if gtt, okGTT := drmSysfsUint(c.deviceDir, "mem_info_gtt_used"); okGTT {
			used += gtt
		}
	}
	return used, true
}

// amdTempSensor is one temp<N>_input under a card's hwmon, with its lowercase
// label ("edge", "junction", "mem") or "" when unlabelled.
type amdTempSensor struct {
	input string
	label string
}

// amdEdgeTempC returns the card's edge temperature in whole degrees Celsius.
// The labelled edge sensor wins wherever it sits; with no labels at all the
// first sensor is used, which on amdgpu is temp1 - the edge sensor.
func amdEdgeTempC(deviceDir string) (uint32, bool) {
	sensors := amdTempSensors(deviceDir)
	for _, s := range sensors {
		if s.label == amdEdgeLabel {
			return parseMillidegrees(readSysfs(s.input))
		}
	}
	if len(sensors) == 0 {
		return 0, false
	}
	return parseMillidegrees(readSysfs(sensors[0].input))
}

// amdTempSensors lists the temperature inputs under <deviceDir>/hwmon, with
// amdgpu's own hwmon first and each hwmon's inputs in temp<N> order, so the
// "first sensor" fallback is deterministic across boots (hwmon numbering is
// not).
func amdTempSensors(deviceDir string) []amdTempSensor {
	var sensors []amdTempSensor
	for _, dir := range amdHwmonDirs(deviceDir) {
		sensors = append(sensors, amdHwmonTempSensors(dir)...)
	}
	return sensors
}

// amdHwmonDirs lists a card's hwmon directories as full paths, amdgpu's own
// first and the rest in name order, so every "first one wins" fallback below
// is deterministic across boots — hwmon numbering is not.
func amdHwmonDirs(deviceDir string) []string {
	hwmonRoot := filepath.Join(deviceDir, "hwmon")
	entries, err := os.ReadDir(hwmonRoot)
	if err != nil {
		return nil
	}
	dirs := make([]string, 0, len(entries))
	for _, e := range entries {
		dirs = append(dirs, e.Name())
	}
	sort.Strings(dirs)
	sort.SliceStable(dirs, func(i, j int) bool {
		return sysfsField(filepath.Join(hwmonRoot, dirs[i], "name")) == amdHwmonName &&
			sysfsField(filepath.Join(hwmonRoot, dirs[j], "name")) != amdHwmonName
	})
	paths := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		paths = append(paths, filepath.Join(hwmonRoot, dir))
	}
	return paths
}

// amdPowerWatts returns the card's socket power in whole watts, from the
// amdgpu hwmon's power1_input (microwatts).
//
// The input labelled PPT wins wherever it sits; with no label at all power1 is
// used, which is what amdgpu registers that attribute as. ok is false on a
// card whose driver publishes no power attribute at all — several older
// discrete parts, and any host where the read fails — so the row omits the
// figure instead of reporting a card that draws nothing.
//
// On an APU this is package power: the CPU cores and the GPU share one socket
// and one budget, and the SMU meters the socket. That is the honest answer for
// the GPU row on such a part, not a defect — there is no separate GPU rail to
// report.
func amdPowerWatts(deviceDir string) (float64, bool) {
	fallback := ""
	for _, hwmonDir := range amdHwmonDirs(deviceDir) {
		input := filepath.Join(hwmonDir, amdPowerInput)
		if readSysfs(input) == "" {
			continue
		}
		if strings.ToLower(sysfsField(filepath.Join(hwmonDir, amdPowerLabel))) == amdPPTLabel {
			return microwattsToWatts(readSysfs(input))
		}
		if fallback == "" {
			fallback = input
		}
	}
	if fallback == "" {
		return 0, false
	}
	return microwattsToWatts(readSysfs(fallback))
}

// microwattsToWatts converts one hwmon power attribute to whole watts. ok is
// false for an unparseable or negative reading, which the driver uses for an
// input it cannot currently sample.
func microwattsToWatts(s string) (float64, bool) {
	uw, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || uw < 0 {
		return 0, false
	}
	return math.Round(float64(uw) / 1e6), true
}

// amdHwmonTempSensors lists one hwmon directory's temp<N>_input files in
// numeric order with their labels resolved.
func amdHwmonTempSensors(hwmonDir string) []amdTempSensor {
	entries, err := os.ReadDir(hwmonDir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "temp") && strings.HasSuffix(e.Name(), "_input") {
			names = append(names, e.Name())
		}
	}
	sort.Slice(names, func(i, j int) bool { return hwmonTempIndex(names[i]) < hwmonTempIndex(names[j]) })
	sensors := make([]amdTempSensor, 0, len(names))
	for _, name := range names {
		label := filepath.Join(hwmonDir, strings.TrimSuffix(name, "_input")+"_label")
		sensors = append(sensors, amdTempSensor{
			input: filepath.Join(hwmonDir, name),
			label: strings.ToLower(sysfsField(label)),
		})
	}
	return sensors
}

// hwmonTempIndex is the N of "temp<N>_input", so temp2 sorts before temp10
// (lexical order would not). An unparseable name sorts last.
func hwmonTempIndex(name string) int {
	digits := strings.TrimSuffix(strings.TrimPrefix(name, "temp"), "_input")
	n := 0
	for _, r := range digits {
		if r < '0' || r > '9' {
			return 1 << 30
		}
		n = n*10 + int(r-'0')
	}
	if digits == "" {
		return 1 << 30
	}
	return n
}
