// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// writeFile writes one file of a fake sysfs tree, creating its parents.
func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeThermalZone lays out <root>/thermal_zone<n>/{type,temp}.
func writeThermalZone(t *testing.T, root string, index int, zoneType, milli string) {
	t.Helper()
	dir := filepath.Join(root, "thermal_zone"+strconv.Itoa(index))
	writeFile(t, filepath.Join(dir, "type"), zoneType+"\n")
	writeFile(t, filepath.Join(dir, "temp"), milli+"\n")
}

// registerRockchipSamplerForTest puts samplers in the process-wide set without
// starting their goroutines, and empties the set afterwards.
func registerRockchipSamplerForTest(t *testing.T, samplers ...*rockchipSampler) {
	t.Helper()
	rockchipSamplerSet.mu.Lock()
	rockchipSamplerSet.list = append(rockchipSamplerSet.list, samplers...)
	rockchipSamplerSet.mu.Unlock()
	t.Cleanup(func() {
		rockchipSamplerSet.mu.Lock()
		rockchipSamplerSet.list = nil
		rockchipSamplerSet.mu.Unlock()
	})
}

// fakeRockchipRoots builds a tempdir tree shaped like the measured RK3588S
// board: the Mali misc device with its gpuinfo and devfreq load, the RKNPU
// devfreq node with its uevent, the gpu-/npu-thermal zones, and the RKNPU
// debugfs load file.
func fakeRockchipRoots(t *testing.T) rockchipRoots {
	t.Helper()
	base := t.TempDir()
	r := rockchipRoots{
		misc:    filepath.Join(base, "class", "misc"),
		devfreq: filepath.Join(base, "class", "devfreq"),
		thermal: filepath.Join(base, "class", "thermal"),
		debugfs: filepath.Join(base, "debug"),
	}

	maliDev := filepath.Join(r.misc, "mali0", "device")
	writeFile(t, filepath.Join(maliDev, "gpuinfo"), "Mali-G610 4 cores r0p0 0xA867\n")
	writeFile(t, filepath.Join(maliDev, "utilisation"), "11\n")
	writeFile(t, filepath.Join(maliDev, "devfreq", "fb000000.gpu", "load"), "37@600000000Hz\n")
	writeFile(t, filepath.Join(r.devfreq, "fb000000.gpu", "load"), "37@600000000Hz\n")

	// The class node's own uevent is empty on the vendor kernel; the driver and
	// device-tree keys live on the platform device behind it.
	writeFile(t, filepath.Join(r.devfreq, "fdab0000.npu", "uevent"), "")
	writeFile(t, filepath.Join(r.devfreq, "fdab0000.npu", "device", "uevent"),
		"DRIVER=RKNPU\nOF_NAME=npu\nOF_FULLNAME=/npu@fdab0000\nOF_COMPATIBLE_0=rockchip,rk3588-rknpu\nOF_COMPATIBLE_N=1\n")
	writeFile(t, filepath.Join(r.devfreq, "fdab0000.npu", "load"), "100@1000000000Hz\n")

	writeThermalZone(t, r.thermal, 0, "soc-thermal", "48000")
	writeThermalZone(t, r.thermal, 5, "gpu-thermal", "45600")
	writeThermalZone(t, r.thermal, 6, "npu-thermal", "44100")

	writeFile(t, filepath.Join(r.debugfs, "rknpu", "load"),
		"NPU load:  Core0:  0%, Core1:  0%, Core2:  0%,\n")
	writeFile(t, filepath.Join(r.debugfs, "rknpu", "version"), "RKNPU driver: v0.9.7\n")
	return r
}

// TestMaliProductName pins the name derived from the driver's gpuinfo text:
// the marketing name plus the MP<cores> suffix, and a generic name for text
// that is not a Mali gpuinfo at all.
func TestMaliProductName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Mali-G610 4 cores r0p0 0xA867\n", "Arm Mali-G610 MP4"},
		{"Mali-G52 2 cores r1p0 0x7212", "Arm Mali-G52 MP2"},
		{"Mali-T860 4 core r2p0", "Arm Mali-T860 MP4"},
		{"Mali-G610", "Arm Mali-G610"},
		{"Mali-G610 0 cores r0p0", "Arm Mali-G610"},
		{"Mali-G610 many cores", "Arm Mali-G610"},
		{"", maliFallbackName},
		{"unexpected driver text", maliFallbackName},
	}
	for _, tc := range cases {
		if got := maliProductName(tc.in); got != tc.want {
			t.Errorf("maliProductName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestParseDevfreqLoad pins the "<busy%>@<freq>Hz" parse: the frequency half is
// dropped, an impossible figure is clamped, and garbage reports "no reading"
// rather than a fabricated 0 %.
func TestParseDevfreqLoad(t *testing.T) {
	cases := []struct {
		in     string
		want   uint32
		wantOK bool
	}{
		{"37@600000000Hz\n", 37, true},
		{"0@1000000000Hz", 0, true},
		{"100@1000000000Hz", 100, true},
		{"255@1000000000Hz", 100, true},
		{"42", 42, true},
		{"", 0, false},
		{"@1000000000Hz", 0, false},
		{"busy@1000000000Hz", 0, false},
		{"-1@600000000Hz", 0, false},
		{"garbage", 0, false},
	}
	for _, tc := range cases {
		got, ok := parseDevfreqLoad(tc.in)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("parseDevfreqLoad(%q) = (%d, %v), want (%d, %v)", tc.in, got, ok, tc.want, tc.wantOK)
		}
	}
}

// TestFindMaliDeviceAndRow walks the fake tree end to end: name from gpuinfo,
// statsKey from the devfreq node, unified memory from the system total, and a
// utilization read that comes back from the devfreq load.
func TestFindMaliDeviceAndRow(t *testing.T) {
	r := fakeRockchipRoots(t)
	dev, ok := findMaliDevice(r)
	if !ok {
		t.Fatal("findMaliDevice found no GPU in the fake tree")
	}
	if dev.name != "Arm Mali-G610 MP4" {
		t.Errorf("name = %q, want %q", dev.name, "Arm Mali-G610 MP4")
	}
	if dev.node != "fb000000.gpu" {
		t.Errorf("devfreq node = %q, want fb000000.gpu", dev.node)
	}
	if dev.statsKey() != "mali:fb000000.gpu" {
		t.Errorf("statsKey = %q, want mali:fb000000.gpu", dev.statsKey())
	}
	if filepath.Base(filepath.Dir(dev.tempPath)) != "thermal_zone5" {
		t.Errorf("thermal path = %q, want the gpu-thermal zone", dev.tempPath)
	}

	const memTotal = 8 * 1024 * 1024 * 1024
	row := maliRow(dev, memTotal)
	if row.Kind != "" {
		t.Errorf("Kind = %q, want empty (a Mali is a GPU, not an accelerator)", row.Kind)
	}
	if !row.usesSystemMemoryUsage || row.VramBytes != memTotal {
		t.Errorf("unified memory not wired: usesSystemMemoryUsage=%v VramBytes=%d", row.usesSystemMemoryUsage, row.VramBytes)
	}

	if util, ok := dev.readUtilization(); !ok || util != 37 {
		t.Errorf("readUtilization = (%d, %v), want (37, true)", util, ok)
	}
	if temp, ok := parseMillidegrees(readSysfs(dev.tempPath)); !ok || temp != 46 {
		t.Errorf("temperature = (%d, %v), want (46, true)", temp, ok)
	}
}

// TestFindMaliDeviceFallbacks pins the degraded shapes: no devfreq under the
// device (the class tree names the node), no devfreq at all (the row keys
// itself by the misc name and reads the driver's utilisation attribute), and no
// Mali device (every non-Rockchip host).
func TestFindMaliDeviceFallbacks(t *testing.T) {
	r := fakeRockchipRoots(t)
	if err := os.RemoveAll(filepath.Join(r.misc, "mali0", "device", "devfreq")); err != nil {
		t.Fatal(err)
	}
	dev, ok := findMaliDevice(r)
	if !ok || dev.node != "fb000000.gpu" {
		t.Fatalf("devfreq class fallback: dev.node = %q, ok = %v", dev.node, ok)
	}
	if util, ok := dev.readUtilization(); !ok || util != 37 {
		t.Errorf("devfreq class load: readUtilization = (%d, %v), want (37, true)", util, ok)
	}

	if err := os.RemoveAll(filepath.Join(r.devfreq, "fb000000.gpu")); err != nil {
		t.Fatal(err)
	}
	dev, ok = findMaliDevice(r)
	if !ok {
		t.Fatal("no devfreq: findMaliDevice returned false, want the row to survive")
	}
	if dev.node != "" || dev.statsKey() != "mali:mali0" {
		t.Errorf("no devfreq: node = %q, statsKey = %q, want \"\" and mali:mali0", dev.node, dev.statsKey())
	}
	if util, ok := dev.readUtilization(); !ok || util != 11 {
		t.Errorf("no devfreq: readUtilization = (%d, %v), want (11, true) from the utilisation attribute", util, ok)
	}

	empty := rockchipRoots{misc: t.TempDir(), devfreq: t.TempDir(), thermal: t.TempDir(), debugfs: t.TempDir()}
	if _, ok := findMaliDevice(empty); ok {
		t.Error("findMaliDevice reported a GPU on a host with no Mali driver")
	}
}

// TestFindMaliDeviceNoThermalZone pins that a board without a gpu-thermal zone
// still lists the GPU; only its temperature is missing.
func TestFindMaliDeviceNoThermalZone(t *testing.T) {
	r := fakeRockchipRoots(t)
	if err := os.RemoveAll(filepath.Join(r.thermal, "thermal_zone5")); err != nil {
		t.Fatal(err)
	}
	dev, ok := findMaliDevice(r)
	if !ok {
		t.Fatal("findMaliDevice returned false without a thermal zone")
	}
	if dev.tempPath != "" {
		t.Errorf("tempPath = %q, want empty", dev.tempPath)
	}
	stat := sampleOnce(newRockchipSampler(dev.statsKey(), dev.readUtilization, dev.tempPath))
	if stat.TemperatureC != 0 || stat.UtilizationPct != 37 {
		t.Errorf("sample = %+v, want utilization 37 and no temperature", stat)
	}
}

// sampleOnce drives one sampling pass without starting the goroutine.
func sampleOnce(s *rockchipSampler) gpuStat {
	s.sample()
	stat, _ := s.Latest()
	return stat
}

// TestRockchipSamplerKeepsLastGoodReading pins that a source that stops reading
// (a driver node that disappears mid-run) preserves the last value instead of
// publishing a fabricated idle sample.
func TestRockchipSamplerKeepsLastGoodReading(t *testing.T) {
	readable := true
	s := newRockchipSampler("mali:test", func() (uint32, bool) {
		if !readable {
			return 0, false
		}
		return 80, true
	}, "")
	if got := sampleOnce(s).UtilizationPct; got != 80 {
		t.Fatalf("first sample = %d, want 80", got)
	}
	readable = false
	if got := sampleOnce(s).UtilizationPct; got != 80 {
		t.Fatalf("after a failed read utilization = %d, want the last good 80", got)
	}
}

// TestMergeRockchipStats pins the snapshot merge: samples land under their
// statsKey, the previous map is never mutated (it may be the already-published
// one), and GPU telemetry freshness is left alone.
func TestMergeRockchipStats(t *testing.T) {
	published := map[string]gpuStat{"GPU-uuid": {UtilizationPct: 12}}
	snap := &statsSnapshot{GPU: published}
	mergeRockchipStats(snap)
	if len(snap.GPU) != 1 {
		t.Fatalf("no samplers registered: GPU map = %v, want it untouched", snap.GPU)
	}

	s := newRockchipSampler("mali:fb000000.gpu", func() (uint32, bool) { return 37, true }, "")
	s.sample()
	registerRockchipSamplerForTest(t, s)

	mergeRockchipStats(snap)
	if got := snap.GPU["mali:fb000000.gpu"].UtilizationPct; got != 37 {
		t.Errorf("merged utilization = %d, want 37", got)
	}
	if snap.GPU["GPU-uuid"].UtilizationPct != 12 {
		t.Error("merge dropped an existing GPU entry")
	}
	if len(published) != 1 {
		t.Errorf("merge mutated the previously published map: %v", published)
	}
	if !snap.GPUSampledAt.IsZero() {
		t.Error("a Rockchip sample must not mark GPU telemetry fresh")
	}
}
