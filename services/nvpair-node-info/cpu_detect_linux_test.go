// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fakeDeviceTree lays out a /proc/device-tree-shaped tree plus a /proc/cpuinfo
// with the given number of processor entries. Device-tree properties are
// NUL-terminated, and compatible is a NUL-separated list, exactly as the kernel
// exposes them.
func fakeDeviceTree(t *testing.T, model string, compatible []string, procs int) cpuFallbackRoots {
	t.Helper()
	base := t.TempDir()
	r := cpuFallbackRoots{
		deviceTree: filepath.Join(base, "device-tree"),
		cpuinfo:    filepath.Join(base, "cpuinfo"),
	}
	if model != "" {
		writeFile(t, filepath.Join(r.deviceTree, "model"), model+"\x00")
	}
	if len(compatible) > 0 {
		writeFile(t, filepath.Join(r.deviceTree, "compatible"), strings.Join(compatible, "\x00")+"\x00")
	}
	var b strings.Builder
	for i := 0; i < procs; i++ {
		b.WriteString("processor\t: " + strconv.Itoa(i) + "\n")
		b.WriteString("BogoMIPS\t: 48.00\nCPU part\t: 0xd0b\n\n")
	}
	writeFile(t, r.cpuinfo, b.String())
	return r
}

// orangePiRoots is the measured board: an 8-core RK3588S whose device tree
// names the board and the SoC.
func orangePiRoots(t *testing.T) cpuFallbackRoots {
	t.Helper()
	return fakeDeviceTree(t, "Orange Pi 5",
		[]string{"rockchip,rk3588s-orangepi-5", "rockchip,rk3588"}, 8)
}

// TestDeviceTreeIdentity pins the two values read from the device tree: the
// board model and the SoC, the latter taken from the first compatible entry
// that actually names a part number rather than a board.
func TestDeviceTreeIdentity(t *testing.T) {
	model, soc := deviceTreeIdentity(orangePiRoots(t).deviceTree)
	if model != "Orange Pi 5" {
		t.Errorf("model = %q, want %q", model, "Orange Pi 5")
	}
	if soc != "Rockchip RK3588S" {
		t.Errorf("soc = %q, want %q", soc, "Rockchip RK3588S")
	}

	// A board whose first compatible entry names the vendor's board, not the
	// SoC: the SoC must come from the entry that does.
	boardFirst := fakeDeviceTree(t, "Generic Board",
		[]string{"boardvendor,generic-board-v2", "rockchip,rk3588"}, 8)
	if _, soc := deviceTreeIdentity(boardFirst.deviceTree); soc != "Rockchip RK3588" {
		t.Errorf("soc = %q, want Rockchip RK3588", soc)
	}

	// No device tree at all (an x86 host).
	if model, soc := deviceTreeIdentity(filepath.Join(t.TempDir(), "missing")); model != "" || soc != "" {
		t.Errorf("missing device tree = (%q, %q), want empty", model, soc)
	}
}

func TestSplitCompatible(t *testing.T) {
	cases := []struct {
		in           string
		vendor, part string
	}{
		{"rockchip,rk3588s-orangepi-5", "Rockchip", "RK3588S"},
		{"rockchip,rk3588", "Rockchip", "RK3588"},
		{"rockchip,rk3588-rknpu", "Rockchip", "RK3588"},
		{"rknpu", "", ""},
		{"", "", ""},
	}
	for _, tc := range cases {
		vendor, part := splitCompatible(tc.in)
		if vendor != tc.vendor || part != tc.part {
			t.Errorf("splitCompatible(%q) = (%q, %q), want (%q, %q)", tc.in, vendor, part, tc.vendor, tc.part)
		}
	}
}

// TestMergeCPUName pins how ghw's name and the device tree combine.
func TestMergeCPUName(t *testing.T) {
	cases := []struct {
		name, ghw, model, soc, want string
	}{
		{
			name:  "ghw empty: board first, SoC qualifies it",
			model: "Orange Pi 5", soc: "Rockchip RK3588S",
			want: "Orange Pi 5 (Rockchip RK3588S)",
		},
		{
			name: "ghw has the SoC: the board is appended",
			ghw:  "Rockchip RK3588S", model: "Orange Pi 5", soc: "Rockchip RK3588S",
			want: "Rockchip RK3588S (Orange Pi 5)",
		},
		{
			name: "ghw already names the board: unchanged",
			ghw:  "Orange Pi 5 (Rockchip RK3588S)", model: "Orange Pi 5", soc: "Rockchip RK3588S",
			want: "Orange Pi 5 (Rockchip RK3588S)",
		},
		{
			name: "no device tree: ghw's answer is kept",
			ghw:  "AMD Ryzen 9 7950X 16-Core Processor",
			want: "AMD Ryzen 9 7950X 16-Core Processor",
		},
		{
			name: "no model, SoC only",
			soc:  "Rockchip RK3588S", want: "Rockchip RK3588S",
		},
		{
			name: "model only", model: "Orange Pi 5", want: "Orange Pi 5",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mergeCPUName(tc.ghw, tc.model, tc.soc); got != tc.want {
				t.Fatalf("mergeCPUName(%q, %q, %q) = %q, want %q", tc.ghw, tc.model, tc.soc, got, tc.want)
			}
		})
	}
}

// TestCPUInfoOrFallbackIn is the hook cpu_detect.go calls, over the three
// shapes that matter: the measured board (ghw names the SoC but counts one
// cluster), a board where ghw found no name at all, and an x86 host with no
// device tree, whose physical-core count must survive untouched even though
// /proc/cpuinfo lists more SMT threads.
func TestCPUInfoOrFallbackIn(t *testing.T) {
	board := orangePiRoots(t)

	got := cpuInfoOrFallbackIn("Rockchip RK3588S", 4, board)
	if got.Name != "Rockchip RK3588S (Orange Pi 5)" {
		t.Errorf("name = %q, want %q", got.Name, "Rockchip RK3588S (Orange Pi 5)")
	}
	if got.Cores != 8 {
		t.Errorf("cores = %d, want 8 (ghw counted one cluster)", got.Cores)
	}

	got = cpuInfoOrFallbackIn("", 0, board)
	if got.Name != "Orange Pi 5 (Rockchip RK3588S)" {
		t.Errorf("empty ghw name = %q, want %q", got.Name, "Orange Pi 5 (Rockchip RK3588S)")
	}
	if got.Cores != 8 {
		t.Errorf("empty ghw cores = %d, want 8", got.Cores)
	}

	x86 := fakeDeviceTree(t, "", nil, 32)
	got = cpuInfoOrFallbackIn("AMD Ryzen 9 7950X 16-Core Processor", 16, x86)
	if got.Name != "AMD Ryzen 9 7950X 16-Core Processor" || got.Cores != 16 {
		t.Errorf("x86 host = %+v, want the ghw answer untouched (16 physical cores)", got)
	}
}

// TestCPUNameFallbackIn pins the device-tree-only identity used when ghw
// reports no name at all.
func TestCPUNameFallbackIn(t *testing.T) {
	name, cores := cpuNameFallbackIn(orangePiRoots(t))
	if name != "Orange Pi 5 (Rockchip RK3588S)" || cores != 8 {
		t.Fatalf("cpuNameFallbackIn = (%q, %d), want (%q, 8)", name, cores, "Orange Pi 5 (Rockchip RK3588S)")
	}
	if name, cores := cpuNameFallbackIn(fakeDeviceTree(t, "", nil, 0)); name != "" || cores != 0 {
		t.Fatalf("no device tree = (%q, %d), want empty", name, cores)
	}
}

func TestCountProcCPUs(t *testing.T) {
	r := orangePiRoots(t)
	if got := countProcCPUs(r.cpuinfo); got != 8 {
		t.Errorf("countProcCPUs = %d, want 8", got)
	}
	if got := countProcCPUs(filepath.Join(t.TempDir(), "missing")); got != 0 {
		t.Errorf("missing /proc/cpuinfo = %d, want 0", got)
	}
}

func TestDTStringHelpers(t *testing.T) {
	if got := dtString("Orange Pi 5\x00"); got != "Orange Pi 5" {
		t.Errorf("dtString = %q", got)
	}
	got := dtStringList("rockchip,rk3588s-orangepi-5\x00rockchip,rk3588\x00")
	if len(got) != 2 || got[0] != "rockchip,rk3588s-orangepi-5" || got[1] != "rockchip,rk3588" {
		t.Errorf("dtStringList = %q", got)
	}
	if got := dtStringList(""); got != nil {
		t.Errorf("dtStringList(\"\") = %q, want nil", got)
	}
}
