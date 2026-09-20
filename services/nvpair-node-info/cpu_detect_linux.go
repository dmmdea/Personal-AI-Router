// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

// Linux CPU identity repair for SoC boards, where ghw alone is wrong or empty.
//
// ghw derives both fields from /proc/cpuinfo, which on Arm prints neither a
// model name nor one entry per cluster the way x86 does. Two failures follow,
// both measured on an RK3588S board:
//
//   - the core count is the size of ONE cluster (4) rather than the SoC's total
//     (8 = 4x Cortex-A76 + 4x Cortex-A55), because the kernel groups asymmetric
//     cores into separate physical packages and ghw reports the first;
//   - the model name is either empty (lscpu prints "-") or, at best, the SoC
//     part number with no indication of which board it is.
//
// The device tree carries what /proc/cpuinfo does not: /proc/device-tree/model
// is the board ("Orange Pi 5") and the first compatible entry names the SoC
// ("rockchip,rk3588s-orangepi-5" -> "Rockchip RK3588S"). So the name is
// repaired rather than replaced — a SoC name ghw did find keeps its place and
// gains the board it is on ("Rockchip RK3588S (Orange Pi 5)"), while an empty
// name is built board-first ("Orange Pi 5 (Rockchip RK3588S)"). The core count
// is raised, never lowered: /proc/cpuinfo lists one "processor" entry per
// online logical CPU, which is the number a user counts on a SoC with no SMT.
//
// All of this is gated on the host having a device tree, which x86 hosts do
// not: there the lookups come back empty and the ghw answer is returned
// untouched — notably its core count, because /proc/cpuinfo would there report
// SMT threads rather than the physical cores this field means.

const (
	procDeviceTreeDir = "/proc/device-tree"
	procCPUInfoPath   = "/proc/cpuinfo"
)

// socTokenPattern recognizes a SoC part number ("rk3588s", "rk3588") as opposed
// to a board or vendor token ("orangepi", "nanopc"), so the SoC is taken from
// the first compatible entry that actually names one.
var socTokenPattern = regexp.MustCompile(`^[a-z]+[0-9]{3,}[a-z0-9]*$`)

// cpuFallbackRoots are the files the repair reads, parameterized so tests can
// point them at a fake tree.
type cpuFallbackRoots struct {
	deviceTree string // /proc/device-tree
	cpuinfo    string // /proc/cpuinfo
}

func systemCPUFallbackRoots() cpuFallbackRoots {
	return cpuFallbackRoots{deviceTree: procDeviceTreeDir, cpuinfo: procCPUInfoPath}
}

// cpuInfoOrFallback is the hook cpu_detect.go calls with ghw's answer. On a
// host with no device tree it hands back exactly what it was given.
func cpuInfoOrFallback(name string, cores uint32) *CPUInfo {
	return cpuInfoOrFallbackIn(name, cores, systemCPUFallbackRoots())
}

func cpuInfoOrFallbackIn(name string, cores uint32, r cpuFallbackRoots) *CPUInfo {
	model, soc := deviceTreeIdentity(r.deviceTree)
	if model == "" && soc == "" {
		// No device tree: an x86 host (or an ACPI Arm server), where ghw's
		// answer is already right. Crucially the core count is left alone —
		// /proc/cpuinfo lists one entry per SMT thread there, so raising the
		// count would replace physical cores with logical processors and
		// contradict what the field has always reported.
		return &CPUInfo{Name: name, Cores: cores}
	}
	out := &CPUInfo{Name: strings.TrimSpace(name), Cores: cores}
	if out.Name == "" {
		out.Name, _ = cpuNameFallbackIn(r)
	} else {
		out.Name = mergeCPUName(out.Name, model, soc)
	}
	if listed := countProcCPUs(r.cpuinfo); listed > out.Cores {
		out.Cores = listed
	}
	return out
}

// cpuNameFallback is the device-tree-only CPU identity: the name to use when
// ghw reported none, and the logical-CPU count it lists. Its non-Linux no-op
// lives in cpu_detect_other.go.
func cpuNameFallback() (name string, cores uint32) {
	return cpuNameFallbackIn(systemCPUFallbackRoots())
}

func cpuNameFallbackIn(r cpuFallbackRoots) (name string, cores uint32) {
	model, soc := deviceTreeIdentity(r.deviceTree)
	return mergeCPUName("", model, soc), countProcCPUs(r.cpuinfo)
}

// mergeCPUName combines ghw's name with the device tree's board model and SoC.
// With no ghw name the board leads and the SoC qualifies it; with a ghw name
// (already the SoC on these boards) the board is appended, unless the name
// already carries it.
func mergeCPUName(ghwName, model, soc string) string {
	ghwName = strings.TrimSpace(ghwName)
	if ghwName == "" {
		switch {
		case model != "" && soc != "":
			return model + " (" + soc + ")"
		case model != "":
			return model
		default:
			return soc
		}
	}
	if model == "" || strings.Contains(strings.ToLower(ghwName), strings.ToLower(model)) {
		return ghwName
	}
	return ghwName + " (" + model + ")"
}

// deviceTreeIdentity reads the board model and the SoC name from the device
// tree. Either can be "" on a host that exposes neither.
func deviceTreeIdentity(root string) (model, soc string) {
	model = dtString(readSysfs(filepath.Join(root, "model")))
	entries := dtStringList(readSysfs(filepath.Join(root, "compatible")))
	for _, entry := range entries {
		if vendor, part := splitCompatible(entry); part != "" && socTokenPattern.MatchString(strings.ToLower(part)) {
			return model, strings.TrimSpace(vendor + " " + part)
		}
	}
	// No entry named a recognizable SoC part; fall back to the first entry's
	// generic vendor,part split so an unknown platform still says something.
	for _, entry := range entries {
		if vendor, part := splitCompatible(entry); part != "" {
			return model, strings.TrimSpace(vendor + " " + part)
		}
	}
	return model, ""
}

// splitCompatible splits a device-tree compatible entry, "vendor,part-board",
// into a capitalized vendor ("Rockchip") and the upper-cased part token
// ("RK3588S"). Both are "" when the entry carries no vendor prefix.
// accel_rockchip_linux.go reuses it to name the NPU from its own compatible
// string ("rockchip,rk3588-rknpu" -> "RK3588").
func splitCompatible(entry string) (vendor, part string) {
	entry = strings.TrimSpace(entry)
	comma := strings.IndexByte(entry, ',')
	if comma < 0 {
		return "", ""
	}
	vendor = capitalizeWord(entry[:comma])
	part = entry[comma+1:]
	if dash := strings.IndexByte(part, '-'); dash >= 0 {
		part = part[:dash]
	}
	return vendor, strings.ToUpper(part)
}

// capitalizeWord upper-cases the first rune of a lowercase vendor token.
func capitalizeWord(s string) string {
	runes := []rune(strings.TrimSpace(s))
	if len(runes) == 0 {
		return ""
	}
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}

// dtString trims a device-tree property to its Go string: the kernel exposes
// these NUL-terminated.
func dtString(s string) string {
	return strings.TrimSpace(strings.TrimRight(s, "\x00"))
}

// dtStringList splits a device-tree string-list property (NUL-separated, e.g.
// compatible) into its entries, dropping empties.
func dtStringList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, "\x00") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// countProcCPUs counts the "processor" entries of /proc/cpuinfo, i.e. the
// online logical CPUs. Returns 0 when the file is unreadable.
func countProcCPUs(path string) uint32 {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var n uint32
	for _, line := range strings.Split(string(data), "\n") {
		key, _, ok := strings.Cut(line, ":")
		if ok && strings.TrimSpace(key) == "processor" {
			n++
		}
	}
	return n
}
