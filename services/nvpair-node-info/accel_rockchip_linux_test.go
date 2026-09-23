// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nvpair-shared/noderec"
)

// TestParseRKNPULoad pins the debugfs load parse for the core counts the
// RK35xx family ships, and the rejection of text that names no core — which
// must read as "no sample" rather than as an idle NPU.
func TestParseRKNPULoad(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		wantMean  uint32
		wantCores int
		wantOK    bool
	}{
		{
			name:      "three idle cores as the driver prints them",
			in:        "NPU load:  Core0:  0%, Core1:  0%, Core2:  0%,\n",
			wantMean:  0,
			wantCores: 3,
			wantOK:    true,
		},
		{
			name:      "three busy cores are averaged",
			in:        "NPU load:  Core0: 90%, Core1: 60%, Core2:  0%,\n",
			wantMean:  50,
			wantCores: 3,
			wantOK:    true,
		},
		{
			name:      "the mean is rounded, not truncated",
			in:        "NPU load:  Core0: 10%, Core1: 20%, Core2: 25%,\n",
			wantMean:  18,
			wantCores: 3,
			wantOK:    true,
		},
		{
			name:      "single-core SoC",
			in:        "NPU load:  Core0: 42%,\n",
			wantMean:  42,
			wantCores: 1,
			wantOK:    true,
		},
		{
			name:      "an impossible per-core figure is clamped",
			in:        "NPU load:  Core0: 400%,\n",
			wantMean:  100,
			wantCores: 1,
			wantOK:    true,
		},
		{name: "empty file", in: ""},
		{name: "header only", in: "NPU load:\n"},
		{name: "foreign text", in: "RKNPU driver: v0.9.7\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mean, cores, ok := parseRKNPULoad(tc.in)
			if ok != tc.wantOK || mean != tc.wantMean || cores != tc.wantCores {
				t.Fatalf("parseRKNPULoad(%q) = (%d, %d, %v), want (%d, %d, %v)",
					tc.in, mean, cores, ok, tc.wantMean, tc.wantCores, tc.wantOK)
			}
		})
	}
}

func TestRKNPUProductName(t *testing.T) {
	cases := []struct {
		compatible string
		rootSoC    string
		cores      int
		want       string
	}{
		{"rockchip,rk3588-rknpu", "", 3, "Rockchip RK3588 NPU (3 cores)"},
		{"rockchip,rk3588-rknpu", "RK3588", 3, "Rockchip RK3588 NPU (3 cores)"},
		// The RK3588S shares the RK3588's NPU block and its compatible; the
		// board's root compatible names the actual chip, as the CPU row does.
		{"rockchip,rk3588-rknpu", "RK3588S", 3, "Rockchip RK3588S NPU (3 cores)"},
		// A root part that is not a variant of the NPU's family (a board token,
		// a different SoC) never renames the NPU.
		{"rockchip,rk3588-rknpu", "RK3568", 3, "Rockchip RK3588 NPU (3 cores)"},
		{"rockchip,rk3588-rknpu", "ORANGEPI", 3, "Rockchip RK3588 NPU (3 cores)"},
		{"rockchip,rk3568-rknpu", "", 1, "Rockchip RK3568 NPU (1 core)"},
		{"rockchip,rk3576-rknpu", "", 0, "Rockchip RK3576 NPU"},
		{"", "RK3588S", 3, rknpuFallbackName},
		{"rknpu", "", 3, rknpuFallbackName},
	}
	for _, tc := range cases {
		if got := rknpuProductName(tc.compatible, tc.rootSoC, tc.cores); got != tc.want {
			t.Errorf("rknpuProductName(%q, %q, %d) = %q, want %q", tc.compatible, tc.rootSoC, tc.cores, got, tc.want)
		}
	}
}

// TestRKNPUNamedFromTheBoardSoC walks the whole path on an RK3588S board: the
// NPU node's compatible says rk3588, the root compatible says rk3588s, and the
// row must name the same chip the CPU row on the same card does.
func TestRKNPUNamedFromTheBoardSoC(t *testing.T) {
	r := fakeRockchipRoots(t)
	r.deviceTree = filepath.Join(t.TempDir(), "device-tree")
	writeFile(t, filepath.Join(r.deviceTree, "model"), "Orange Pi 5\x00")
	writeFile(t, filepath.Join(r.deviceTree, "compatible"), "rockchip,rk3588s-orangepi-5\x00rockchip,rk3588\x00")

	dev, ok := findRKNPUDevice(r)
	if !ok {
		t.Fatal("findRKNPUDevice found no NPU in the fake tree")
	}
	row := rknpuRow(dev, dev.cores(), 0)
	if row.Name != "Rockchip RK3588S NPU (3 cores)" {
		t.Errorf("name = %q, want the board's RK3588S", row.Name)
	}
	if _, soc := deviceTreeIdentity(r.deviceTree); soc != "Rockchip RK3588S" {
		t.Errorf("the CPU row's SoC = %q; the two rows must agree on RK3588S", soc)
	}
}

// TestFindRKNPUDeviceAndRow walks the fake tree: the devfreq entry is found by
// its uevent driver, the core count comes from debugfs, and the row is an
// accelerator with unified memory. It also pins that the devfreq load — which
// the driver reports as a permanent 100 % — is never the utilization source.
func TestFindRKNPUDeviceAndRow(t *testing.T) {
	r := fakeRockchipRoots(t)
	dev, ok := findRKNPUDevice(r)
	if !ok {
		t.Fatal("findRKNPUDevice found no NPU in the fake tree")
	}
	if dev.node != "fdab0000.npu" || dev.compatible != "rockchip,rk3588-rknpu" {
		t.Fatalf("node = %q, compatible = %q", dev.node, dev.compatible)
	}
	if filepath.Base(filepath.Dir(dev.tempPath)) != "thermal_zone6" {
		t.Errorf("thermal path = %q, want the npu-thermal zone", dev.tempPath)
	}

	const memTotal = 8 * 1024 * 1024 * 1024
	row := rknpuRow(dev, dev.cores(), memTotal)
	if row.Name != "Rockchip RK3588 NPU (3 cores)" {
		t.Errorf("name = %q", row.Name)
	}
	if row.Kind != noderec.GPUKindAccelerator {
		t.Errorf("Kind = %q, want %q", row.Kind, noderec.GPUKindAccelerator)
	}
	if row.statsKey != "rknpu:fdab0000.npu" {
		t.Errorf("statsKey = %q, want rknpu:fdab0000.npu", row.statsKey)
	}
	if row.MemoryPool != noderec.GPUMemoryPoolUnified || row.VramBytes != memTotal {
		t.Errorf("unified pool not wired: MemoryPool=%q VramBytes=%d", row.MemoryPool, row.VramBytes)
	}
	// The NPU allocates from a pool it shares with everything else on the SoC
	// and no driver counter says how much of it the NPU holds. Borrowing the
	// host's usage is what printed "VRAM 1.1 GB / 8 GB" for an idle NPU, so
	// the row must carry no used figure at all.
	if row.usesSystemMemoryUsage {
		t.Error("usesSystemMemoryUsage set on an RKNPU row: /proc/meminfo counts every process on the board, not the NPU")
	}
	if row.VramUsedBytes != 0 {
		t.Errorf("VramUsedBytes = %d, want 0/absent: nothing measures this device's share of the pool", row.VramUsedBytes)
	}
	if noderec.MaxGPUUtilization([]noderec.GPUInfo{{Kind: row.Kind, UtilizationPercent: 100}}) != 0 {
		t.Error("an NPU row must not contribute to GPU pressure")
	}
	if row.InferenceReady == nil || *row.InferenceReady {
		t.Errorf("InferenceReady = %v, want an explicit false: no engine drives an RKNPU", row.InferenceReady)
	}
	if !row.utilizationNeedsSample {
		t.Error("utilizationNeedsSample unset: an unreadable debugfs counter would publish as idle")
	}

	// The device's own devfreq load says 100 % while the NPU is idle; the
	// debugfs counter says 0 %. The sampler must report the counter.
	if util, ok := dev.readUtilization(); !ok || util != 0 {
		t.Errorf("readUtilization = (%d, %v), want (0, true) from the debugfs counter", util, ok)
	}
}

// TestFindRKNPUDeviceUnreadableLoad pins the unprivileged-debugfs path: the row
// survives with its temperature and its SoC core count from the table, and
// utilization reports "no reading" instead of a fabricated 0 %.
func TestFindRKNPUDeviceUnreadableLoad(t *testing.T) {
	r := fakeRockchipRoots(t)
	loadPath := filepath.Join(r.debugfs, "rknpu", "load")
	if err := os.Chmod(loadPath, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(loadPath, 0o644) })
	if _, err := os.ReadFile(loadPath); err == nil {
		t.Skip("this test user can read a 0o000 file (running as root); the EACCES path is unreachable here")
	}

	dev, ok := findRKNPUDevice(r)
	if !ok {
		t.Fatal("findRKNPUDevice returned false with an unreadable load counter")
	}
	if cores := dev.cores(); cores != 3 {
		t.Errorf("cores = %d, want 3 from the compatible-string table", cores)
	}
	row := rknpuRow(dev, dev.cores(), 0)
	if row.Name != "Rockchip RK3588 NPU (3 cores)" {
		t.Errorf("name = %q, want the row to survive an unreadable counter", row.Name)
	}
	if util, ok := dev.readUtilization(); ok || util != 0 {
		t.Errorf("readUtilization = (%d, %v), want (0, false)", util, ok)
	}

	// Temperature-only reporting: the sampler still publishes the thermal zone.
	stat := sampleOnce(newRockchipSampler(row.statsKey, dev.readUtilization, dev.tempPath))
	if stat.TemperatureC != 44 {
		t.Errorf("temperature = %d, want 44", stat.TemperatureC)
	}
	if stat.UtilizationPct != 0 || stat.UtilizationKnown {
		t.Errorf("utilization = %d (known %v), want it omitted and not known", stat.UtilizationPct, stat.UtilizationKnown)
	}

	// And the response says the row has no utilization source, rather than
	// leaving an absent field a client reads as an idle NPU.
	body := buildResponseAt([]GPUInfo{row}, nil, 0, statsSnapshot{GPU: map[string]gpuStat{row.statsKey: stat}}, "", nil, time.Now())
	if !strings.Contains(string(body), `"utilization_unavailable":true`) {
		t.Errorf("response = %s, want utilization_unavailable on the unread NPU", body)
	}
}

// TestFindRKNPUDeviceClassUevent pins the fallback for a kernel that populates
// the devfreq class node's own uevent instead of leaving it empty.
func TestFindRKNPUDeviceClassUevent(t *testing.T) {
	r := fakeRockchipRoots(t)
	node := filepath.Join(r.devfreq, "fdab0000.npu")
	if err := os.RemoveAll(filepath.Join(node, "device")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(node, "uevent"),
		"DRIVER=RKNPU\nOF_COMPATIBLE_0=rockchip,rk3588-rknpu\n")
	dev, ok := findRKNPUDevice(r)
	if !ok || dev.compatible != "rockchip,rk3588-rknpu" {
		t.Fatalf("findRKNPUDevice = (%+v, %v), want the class-node uevent to be used", dev, ok)
	}
}

// TestFindRKNPUDeviceAbsent pins the common case: a devfreq class with no RKNPU
// entry (or none at all) reports no NPU.
func TestFindRKNPUDeviceAbsent(t *testing.T) {
	r := fakeRockchipRoots(t)
	if err := os.RemoveAll(filepath.Join(r.devfreq, "fdab0000.npu")); err != nil {
		t.Fatal(err)
	}
	if _, ok := findRKNPUDevice(r); ok {
		t.Error("findRKNPUDevice reported an NPU after its devfreq entry was removed")
	}
	if _, ok := findRKNPUDevice(rockchipRoots{devfreq: filepath.Join(t.TempDir(), "missing")}); ok {
		t.Error("findRKNPUDevice reported an NPU with no devfreq class at all")
	}
}

func TestUeventValue(t *testing.T) {
	const uevent = "DRIVER=RKNPU\nOF_NAME=npu\nOF_COMPATIBLE_0=rockchip,rk3588-rknpu\n"
	if got := ueventValue(uevent, "DRIVER"); got != "RKNPU" {
		t.Errorf("DRIVER = %q", got)
	}
	if got := ueventValue(uevent, "OF_COMPATIBLE_0"); got != "rockchip,rk3588-rknpu" {
		t.Errorf("OF_COMPATIBLE_0 = %q", got)
	}
	if got := ueventValue(uevent, "MISSING"); got != "" {
		t.Errorf("MISSING = %q, want empty", got)
	}
	if got := ueventValue("", "DRIVER"); got != "" {
		t.Errorf("empty uevent DRIVER = %q, want empty", got)
	}
}
