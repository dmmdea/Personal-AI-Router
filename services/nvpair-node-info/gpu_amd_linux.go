// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"nvpair-shared/gpunames"
	"nvpair-shared/noderec"
)

// Linux AMD GPU inventory, read straight from the amdgpu driver's sysfs nodes.
//
// The NVIDIA path (gpu_linux.go) gets name, capacity and a join key from
// nvidia-smi. AMD ships no equivalent tool in a default install, so a Radeon
// host used to fall through to ghw, which reports the PCI database's codename
// ("Barcelo") and nothing else: no capacity, no join key, and therefore no
// dynamic VRAM / utilization / temperature either.
//
// Everything needed is already in sysfs, world-readable, with no daemon and no
// cgo:
//
//	/sys/class/drm/card<N>/device/
//	    vendor, device              PCI ids ("0x1002", "0x15e7")
//	    uevent                      DRIVER=, PCI_SLOT_NAME=
//	    mem_info_vram_total         dedicated VRAM, bytes
//	    mem_info_gtt_total          GTT (system memory the GPU may map), bytes
//	    mem_info_vram_used          \ sampled every collector tick
//	    mem_info_gtt_used           /
//	    gpu_busy_percent            0..100 busy fraction
//	    hwmon/hwmon<N>/temp*_input  millidegrees Celsius, named by temp*_label
//
// On an APU the "VRAM" figure is only the carve-out the firmware reserved
// (512 MiB on the Ryzen 5 5625U this was measured on) while the real ceiling
// for a model is that carve-out plus the GTT aperture, so capacity is reported
// as vram_total + gtt_total for those parts and as vram_total alone for a
// discrete card. That pool is shared with system RAM, so an APU row is stamped
// noderec.GPUMemoryPoolUnified and a client presents it as a shared ceiling
// rather than as dedicated VRAM.
//
// An APU row is nonetheless the one unified row that keeps a used figure, and
// it is the driver's own: amdgpu measures vram_used + gtt_used for this device
// specifically. That is a different thing from GPUInfo.usesSystemMemoryUsage,
// which substitutes whole-host RAM usage and belongs only to the nvidia UMA
// rows; an AMD row must never set it, or a real per-device measurement would
// be overwritten by a figure describing every process on the box.
//
// Sampling lives in stats_amd_linux.go. Both halves derive their join key and
// their unified/discrete verdict from listAMDCards, so a row and its samples
// can never disagree.

const (
	drmClassDir = "/sys/class/drm"

	// amdPCIVendor is AMD/ATI's PCI vendor id as the sysfs attribute spells it.
	amdPCIVendor = "0x1002"

	// amdStatsKeyPrefix namespaces the statsKey so it can never collide with an
	// nvidia-smi UUID or an "apex:" accelerator key. The "amd:<pci address>"
	// form is shared with the upstream amdgpu inventory work so both
	// implementations join against the same key.
	amdStatsKeyPrefix = "amd:"

	// amdAPUVRAMCeiling is the dedicated-VRAM figure below which a card absent
	// from the shared table is treated as an APU carve-out rather than a discrete
	// card's real memory. No discrete Radeon ships with under 1 GiB; every APU
	// carve-out observed is 512 MiB or less.
	amdAPUVRAMCeiling = 1 << 30
)

// The AMD id -> name table and the Graphics Core IP -> architecture map live
// in nvpair-shared/gpunames, because the Windows inventory has to reach the
// same answer from a DXGI adapter's VendorID/DeviceID pair. This file keeps
// the sysfs half: it reads the ip_discovery tree and passes the numbers in.

// amdGCIPPath is where amdgpu publishes the Graphics Core IP version it read
// out of the ASIC's own IP discovery table, relative to a card's device
// directory. Present on Renoir and every later part (measured: GC 9.3.0 on a
// Barcelo APU, which is gfx90c).
const amdGCIPPath = "ip_discovery/die/0/GC/0"

// amdCard is one enumerated AMD adapter: where its attributes live, what it
// is, and the key its samples are published under.
type amdCard struct {
	card      string // DRM node name, e.g. "card1"
	index     int    // the N of card<N>, for a stable numeric inventory order
	deviceDir string // <drmRoot>/card<N>/device
	deviceID  string // PCI device id, lowercase hex without "0x", e.g. "15e7"

	// unifiedPool marks a row whose capacity is the VRAM carve-out plus the
	// GTT aperture (an APU) rather than dedicated memory (a discrete card).
	// It becomes GPUInfo.MemoryPool on the wire. Deliberately separate from
	// GPUInfo.usesSystemMemoryUsage: this pool's usage comes from the driver,
	// not from /proc/meminfo.
	unifiedPool bool

	// statsKey is "amd:<pci address>", the join key between the static row and
	// the collector's per-tick sample.
	statsKey string
}

// amdDRMUnreadable logs the "no /sys/class/drm at all" case once. A host with
// no amdgpu (an NVIDIA-only box, a container without sysfs) must stay silent
// tick after tick.
var amdDRMUnreadable sync.Once

// detectAMDGPUs returns one inventory row per AMD adapter under drmRoot, in
// card-number order. Capacity follows the unified/discrete rule described at
// the top of this file; the dynamic fields are left zero for buildResponse to
// fill from the collector's snapshot under statsKey.
func detectAMDGPUs(drmRoot string) []GPUInfo {
	var out []GPUInfo
	for _, c := range listAMDCards(drmRoot) {
		row := GPUInfo{
			Name:      amdModelName(c.deviceID, c.deviceDir),
			VramBytes: amdCapacityBytes(c),
			statsKey:  c.statsKey,
		}
		if c.unifiedPool {
			row.MemoryPool = noderec.GPUMemoryPoolUnified
		}
		out = append(out, row)
	}
	return out
}

// listAMDCards enumerates the AMD adapters under drmRoot. Only card<N>
// directories are considered: the DRM class also holds one card<N>-<CONNECTOR>
// entry per output (card1-DP-1, card1-HDMI-A-1, ...) plus renderD<N> nodes,
// all of which point back at the same device and would otherwise list the same
// GPU several times. Non-AMD vendors are skipped, so this is safe to call on
// any host.
func listAMDCards(drmRoot string) []amdCard {
	entries, err := os.ReadDir(drmRoot)
	if err != nil {
		amdDRMUnreadable.Do(func() {
			slog.Debug("DRM class dir unreadable; no AMD GPU inventory", "dir", drmRoot, "err", err)
		})
		return nil
	}
	var cards []amdCard
	for _, e := range entries {
		index, ok := drmCardIndex(e.Name())
		if !ok {
			continue
		}
		deviceDir := filepath.Join(drmRoot, e.Name(), "device")
		if !strings.EqualFold(sysfsField(filepath.Join(deviceDir, "vendor")), amdPCIVendor) {
			continue
		}
		c := amdCard{
			card:      e.Name(),
			index:     index,
			deviceDir: deviceDir,
			deviceID:  amdDeviceID(deviceDir),
		}
		c.unifiedPool = amdUnifiedPool(c)
		c.statsKey = drmPCIStatsKey(amdStatsKeyPrefix, deviceDir, e.Name())
		cards = append(cards, c)
	}
	sort.Slice(cards, func(i, j int) bool { return cards[i].index < cards[j].index })
	return cards
}

// drmCardIndex reports the N of a "card<N>" DRM node name. It rejects the
// connector directories ("card1-DP-1") and every other class entry.
func drmCardIndex(name string) (int, bool) {
	digits, ok := strings.CutPrefix(name, "card")
	if !ok || digits == "" {
		return 0, false
	}
	n, err := strconv.Atoi(digits)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// amdDeviceID returns the PCI device id as lowercase hex without the "0x"
// prefix ("15e7"), or "" when the attribute is missing or malformed.
func amdDeviceID(deviceDir string) string {
	id, ok := strings.CutPrefix(strings.ToLower(sysfsField(filepath.Join(deviceDir, "device"))), "0x")
	if !ok || !isHex(id) {
		return ""
	}
	return id
}

// amdModelName resolves a card to its display name. A listed id gets the
// table's full "<marketing name> (<codename>, <architecture>)" string.
//
// An unlisted id - a part released after the shared table was written - is
// never dropped and never published as a bare codename. It keeps its device id
// so the card is still identifiable, and it gains the architecture family
// whenever the driver's IP discovery table can supply one, which is the whole
// point of reading it: a node listing "AMD Radeon Graphics (device 0x1114,
// RDNA 3.5)" is useful on day one of a new part, where "Krackan" is not.
//
// The IP discovery tree is only read for an id the table does not know, so a
// listed part's published name never depends on which source was readable.
func amdModelName(deviceID, deviceDir string) string {
	id, ok := gpunames.ParseHexID(deviceID)
	if !ok {
		return gpunames.AMDName(0, false, "")
	}
	if name, listed := gpunames.AMD(id); listed {
		return name
	}
	arch, _ := amdGCArchitecture(deviceDir)
	return gpunames.AMDName(id, true, arch)
}

// amdGCArchitecture reads the card's Graphics Core IP version out of amdgpu's
// ip_discovery tree and maps it to an architecture family. ok is false when
// the tree is absent (every pre-Renoir part), unreadable, or carries a version
// the shared table deliberately does not name - in all three cases the caller
// omits the architecture rather than guessing one.
func amdGCArchitecture(deviceDir string) (string, bool) {
	if deviceDir == "" {
		return "", false
	}
	major, haveMajor := drmSysfsUint(deviceDir, filepath.Join(amdGCIPPath, "major"))
	minor, haveMinor := drmSysfsUint(deviceDir, filepath.Join(amdGCIPPath, "minor"))
	if !haveMajor || !haveMinor {
		return "", false
	}
	revision, _ := drmSysfsUint(deviceDir, filepath.Join(amdGCIPPath, "revision"))
	arch, ok := gpunames.AMDArchitecture(major, minor)
	slog.Debug("amdgpu GC IP version",
		"device_dir", deviceDir, "major", major, "minor", minor, "revision", revision,
		"architecture", arch, "named", ok)
	return arch, ok
}

// amdUnifiedPool reports whether this card's usable memory is the VRAM
// carve-out plus the GTT aperture. A listed part is taken at its word; an
// unlisted one is judged by its own numbers, because no discrete Radeon
// exposes under 1 GiB of VRAM.
func amdUnifiedPool(c amdCard) bool {
	if id, ok := gpunames.ParseHexID(c.deviceID); ok {
		if apu, known := gpunames.AMDAPU(id); known {
			return apu
		}
	}
	vram, haveVRAM := drmSysfsUint(c.deviceDir, "mem_info_vram_total")
	_, haveGTT := drmSysfsUint(c.deviceDir, "mem_info_gtt_total")
	return haveVRAM && haveGTT && vram < amdAPUVRAMCeiling
}

// amdCapacityBytes is the card's reportable vram_bytes: carve-out + GTT on a
// unified part, dedicated VRAM on a discrete card. Zero when the driver
// reports neither, which omitempty then drops from the wire.
func amdCapacityBytes(c amdCard) uint64 {
	vram, _ := drmSysfsUint(c.deviceDir, "mem_info_vram_total")
	if !c.unifiedPool {
		return vram
	}
	gtt, _ := drmSysfsUint(c.deviceDir, "mem_info_gtt_total")
	return vram + gtt
}

// drmPCIStatsKey builds a "<prefix><pci address>" join key for a DRM card.
// The class entry's `device` is a symlink into /sys/bus/pci/devices, so the
// resolved directory name is the address; uevent's PCI_SLOT_NAME is the
// fallback for a sysfs view where the link cannot be resolved, and the DRM
// node name is the last resort so a card is never published without a key
// (which would cost it every dynamic field).
//
// Shared by the amdgpu ("amd:") and Intel ("intel:") inventories and by their
// samplers, so a row and its samples can never key differently.
func drmPCIStatsKey(prefix, deviceDir, card string) string {
	if resolved, err := filepath.EvalSymlinks(deviceDir); err == nil {
		if base := filepath.Base(resolved); isPCIAddress(base) {
			return prefix + base
		}
	}
	if slot := ueventValue(readSysfs(filepath.Join(deviceDir, "uevent")), "PCI_SLOT_NAME"); isPCIAddress(slot) {
		return prefix + slot
	}
	return prefix + card
}

// ueventValue pulls one KEY=value line out of a sysfs uevent file.
func ueventValue(uevent, key string) string {
	for _, line := range strings.Split(uevent, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), key+"="); ok {
			return v
		}
	}
	return ""
}

// isPCIAddress reports whether s has the domain:bus:device.function shape
// ("0000:04:00.0"). Checked so an unexpected sysfs layout produces the
// card-name fallback instead of a nonsense key.
func isPCIAddress(s string) bool {
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return false
	}
	devFn := strings.Split(parts[2], ".")
	if len(devFn) != 2 {
		return false
	}
	for _, field := range []string{parts[0], parts[1], devFn[0], devFn[1]} {
		if !isHex(field) {
			return false
		}
	}
	return true
}

// isHex reports whether s is a non-empty run of hexadecimal digits.
func isHex(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// sysfsField reads a sysfs attribute and trims the trailing newline the kernel
// appends to every one of them.
func sysfsField(path string) string {
	return strings.TrimSpace(readSysfs(path))
}

// drmSysfsUint reads a decimal unsigned attribute from a card's device
// directory. ok is false when the file is missing, empty or not a number, so
// callers can tell "zero" from "unknown".
func drmSysfsUint(deviceDir, attr string) (uint64, bool) {
	v, err := strconv.ParseUint(sysfsField(filepath.Join(deviceDir, attr)), 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
