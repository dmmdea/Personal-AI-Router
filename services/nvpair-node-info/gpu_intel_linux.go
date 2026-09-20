// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Linux Intel GPU inventory, read from the i915 / xe drivers' sysfs nodes.
//
// Intel adapters had no detector at all. nvidia-smi does not see them, ghw did
// (by name only) but never ran on a host that had any NVIDIA card, because the
// old detector chain returned at the first source that produced anything. The
// measured case: a node with an NVIDIA A2 on card0 and a CoffeeLake-S GT2
// (UHD Graphics 630, 8086:3e98, driver i915) on card1 published exactly one
// GPU. The machine has two. gpu_linux.go now composes the detectors instead of
// racing them, and this file is the Intel member of that composition.
//
// What sysfs gives us, per card, all world-readable and with no daemon, no
// root and no cgo:
//
//	/sys/class/drm/card<N>/device/
//	    vendor, device              PCI ids ("0x8086", "0x3e98")
//	    driver -> .../drivers/i915  the bound driver, as a symlink
//	    uevent                      DRIVER=, PCI_SLOT_NAME=
//	    mem_info_vram_total         dedicated VRAM on a discrete card only
//	    hwmon/hwmon<N>/temp1_input  millidegrees Celsius, Arc cards only
//
// and, just as importantly, what it does NOT give us:
//
//   - No busy percentage. i915 keeps its engine-busy counters in a PMU
//     exposed through perf_event_open, which is root-gated by default
//     (perf_event_paranoid), not in sysfs. There is no unprivileged sysfs
//     attribute equivalent to amdgpu's gpu_busy_percent. So an Intel row
//     carries NO utilization_percent at all rather than a fabricated 0 —
//     "idle" and "we cannot tell" must never render the same.
//   - No temperature on an integrated GPU. The only sensor near it is the CPU
//     package sensor, which is the CPU's reading and is already published as
//     cpu.temperature_celsius; repeating it as the GPU's would be a lie about
//     a different piece of silicon. A discrete Arc card has its own hwmon, and
//     that one is read.
//
// Memory. An integrated Intel GPU has no dedicated VRAM: it allocates out of
// system DRAM, exactly like the Mali rows in gpu_rockchip_linux.go. Those rows
// are therefore marked usesSystemMemoryUsage and carry the system-memory total
// as VramBytes, so a consumer sees the real ceiling and the real usage instead
// of a zero. A discrete card reports its own mem_info_vram_total when the
// driver exposes one; i915 does not expose it for an iGPU (verified on the
// measured host: the attribute is absent), and an unknown capacity stays 0,
// which omitempty drops from the wire.

const (
	// intelPCIVendor is Intel's PCI vendor id as the sysfs attribute spells it.
	intelPCIVendor = "0x8086"

	// intelStatsKeyPrefix namespaces the statsKey so it can never collide with
	// an nvidia-smi UUID, an "amd:" row or an "apex:" accelerator key. Same
	// "<vendor>:<pci address>" shape as the amdgpu inventory, built by the same
	// helper (drmPCIStatsKey).
	intelStatsKeyPrefix = "intel:"

	// intelFallbackName is the vendor-level name an unlisted adapter falls back
	// to, and the whole name for a card whose device id sysfs could not read.
	intelFallbackName = "Intel Graphics"
)

// intelDrivers are the kernel drivers that own a modern Intel GPU: i915 for
// everything up to and including Meteor Lake plus the Arc A-series, xe for
// Lunar Lake, Battlemage and later. A card bound to neither (vfio-pci for a
// passed-through GPU, or no driver at all) is skipped: it is not this host's
// GPU to report on, and none of the attributes below would be readable.
var intelDrivers = map[string]bool{"i915": true, "xe": true}

// intelModel is one known PCI device id: the name to publish, and whether the
// part is a discrete card (dedicated VRAM) rather than an integrated GPU
// sharing system DRAM.
type intelModel struct {
	name     string
	discrete bool
}

// intelModels maps a PCI device id (lowercase hex, no "0x") to the name this
// service publishes, in the same "<marketing name> (<codename>, <graphics
// architecture>)" shape the amdgpu table uses, so rows from the two vendors
// read alike in one node list.
//
// Every id was checked against the kernel's own id header
// (include/drm/intel/pciids.h, which is what i915 and xe bind on) and against
// the PCI ID Repository's pci.ids for the marketing name — not recalled. Two
// results worth knowing, because the obvious guess is wrong both times:
//
//   - 0x9a60/0x9a68/0x9a70 are INTEL_TGL_GT1_IDS, and pci.ids names them
//     "TigerLake-H GT1 [UHD Graphics]". They are not Iris Xe. Iris Xe on Tiger
//     Lake is the GT2 part (0x9a49, 0x9a40), which is listed separately below.
//   - 0x7d55 is "Meteor Lake-P [Intel Arc Graphics]" but 0x7dd5 is "Meteor
//     Lake-P [Intel Graphics]" — the same generation, sold under two names
//     depending on the Xe-core count, so they do not share a string.
//
// An unlisted id still produces a row — see intelModelName — so a part
// released after this table was written is never dropped from the inventory.
var intelModels = map[string]intelModel{
	// Gen 9.5 (Coffee Lake). 0x3e91/0x3e92/0x3e98 are CFL-S GT2, 0x3e9b is
	// CFL-H GT2; all four are sold as UHD Graphics 630.
	"3e91": {"Intel UHD Graphics 630 (Coffee Lake, Gen 9.5)", false},
	"3e92": {"Intel UHD Graphics 630 (Coffee Lake, Gen 9.5)", false},
	"3e98": {"Intel UHD Graphics 630 (Coffee Lake, Gen 9.5)", false},
	"3e9b": {"Intel UHD Graphics 630 (Coffee Lake, Gen 9.5)", false},

	// Xe-LP (Tiger Lake). GT1 is UHD Graphics, GT2 is Iris Xe.
	"9a60": {"Intel UHD Graphics (Tiger Lake, Xe-LP)", false},
	"9a68": {"Intel UHD Graphics (Tiger Lake, Xe-LP)", false},
	"9a70": {"Intel UHD Graphics (Tiger Lake, Xe-LP)", false},
	"9a40": {"Intel Iris Xe Graphics (Tiger Lake, Xe-LP)", false},
	"9a49": {"Intel Iris Xe Graphics (Tiger Lake, Xe-LP)", false},

	// Xe-LP (Alder Lake / Raptor Lake).
	"46a6": {"Intel Iris Xe Graphics (Alder Lake, Xe-LP)", false},
	"46a8": {"Intel Iris Xe Graphics (Alder Lake, Xe-LP)", false},
	"46aa": {"Intel Iris Xe Graphics (Alder Lake, Xe-LP)", false},
	"a7a0": {"Intel Iris Xe Graphics (Raptor Lake, Xe-LP)", false},
	"a7a1": {"Intel Iris Xe Graphics (Raptor Lake, Xe-LP)", false},

	// Xe-HPG (Alchemist / DG2): the first discrete Arc cards.
	"56a0": {"Intel Arc A770 (Alchemist, Xe-HPG)", true},
	"56a1": {"Intel Arc A750 (Alchemist, Xe-HPG)", true},

	// Xe-LPG (Meteor Lake).
	"7d55": {"Intel Arc Graphics (Meteor Lake, Xe-LPG)", false},
	"7dd5": {"Intel Graphics (Meteor Lake, Xe-LPG)", false},

	// Xe2 (Lunar Lake integrated, Battlemage discrete).
	"64a0": {"Intel Arc Graphics 130V/140V (Lunar Lake, Xe2)", false},
	"e20b": {"Intel Arc B580 (Battlemage, Xe2-HPG)", true},
}

// intelCard is one enumerated Intel adapter: where its attributes live, what
// it is, and the key its samples are published under.
type intelCard struct {
	card      string // DRM node name, e.g. "card1"
	index     int    // the N of card<N>, for a stable numeric inventory order
	deviceDir string // <drmRoot>/card<N>/device
	deviceID  string // PCI device id, lowercase hex without "0x", e.g. "3e98"
	driver    string // bound kernel driver, "i915" or "xe"

	// discrete marks a card with its own VRAM. An integrated GPU allocates out
	// of system DRAM and its row is published as unified memory instead.
	discrete bool

	// statsKey is "intel:<pci address>", the join key between the static row
	// and the collector's per-tick sample.
	statsKey string
}

// intelDRMUnreadable logs the "no /sys/class/drm at all" case once, for the
// same reason listAMDCards does: a host without sysfs must stay silent tick
// after tick rather than logging every second forever.
var intelDRMUnreadable sync.Once

// detectIntelGPUs returns one inventory row per Intel adapter under drmRoot,
// in card-number order. Utilization and (on an iGPU) temperature are left
// absent on purpose — see the ceilings at the top of this file — so the only
// dynamic field an Intel row can gain is a discrete Arc card's temperature,
// merged in by mergeIntelStats.
func detectIntelGPUs(drmRoot string) []GPUInfo {
	var out []GPUInfo
	for _, c := range listIntelCards(drmRoot) {
		row := GPUInfo{
			Name:     intelModelName(c.deviceID),
			statsKey: c.statsKey,
		}
		if c.discrete {
			// Only if the driver actually publishes it: i915 does not expose
			// this attribute for a discrete card on every kernel, and an
			// invented capacity is worse than an omitted one.
			row.VramBytes, _ = drmSysfsUint(c.deviceDir, "mem_info_vram_total")
		} else {
			row.VramBytes = systemMemTotal()
			row.usesSystemMemoryUsage = true
		}
		slog.Debug("Intel GPU detected",
			"name", row.Name, "stats_key", row.statsKey, "driver", c.driver,
			"discrete", c.discrete, "vram_bytes", row.VramBytes)
		out = append(out, row)
	}
	return out
}

// listIntelCards enumerates the Intel adapters under drmRoot. Only card<N>
// directories are considered: the DRM class also holds one card<N>-<CONNECTOR>
// entry per output (card1-DP-1, card1-HDMI-A-1, ...) plus renderD<N> nodes,
// all of which point back at the same device and would otherwise list the same
// GPU several times — and an Intel iGPU is usually the card that owns every
// display connector on the box, so this is not a theoretical concern here.
//
// Two filters, both required. The vendor id keeps the NVIDIA and AMD cards on
// the same host out of this inventory (they have their own detectors, with
// real telemetry). The bound driver keeps out Intel PCI display devices this
// host does not drive.
func listIntelCards(drmRoot string) []intelCard {
	entries, err := os.ReadDir(drmRoot)
	if err != nil {
		intelDRMUnreadable.Do(func() {
			slog.Debug("DRM class dir unreadable; no Intel GPU inventory", "dir", drmRoot, "err", err)
		})
		return nil
	}
	var cards []intelCard
	for _, e := range entries {
		index, ok := drmCardIndex(e.Name())
		if !ok {
			continue
		}
		deviceDir := filepath.Join(drmRoot, e.Name(), "device")
		if !strings.EqualFold(sysfsField(filepath.Join(deviceDir, "vendor")), intelPCIVendor) {
			continue
		}
		driver := drmDriverName(deviceDir)
		if !intelDrivers[driver] {
			slog.Debug("skipping Intel PCI display device with no i915/xe driver bound",
				"card", e.Name(), "driver", driver)
			continue
		}
		c := intelCard{
			card:      e.Name(),
			index:     index,
			deviceDir: deviceDir,
			deviceID:  intelDeviceID(deviceDir),
			driver:    driver,
		}
		c.discrete = intelDiscrete(c)
		c.statsKey = drmPCIStatsKey(intelStatsKeyPrefix, deviceDir, e.Name())
		cards = append(cards, c)
	}
	sort.Slice(cards, func(i, j int) bool { return cards[i].index < cards[j].index })
	return cards
}

// intelDeviceID returns the PCI device id as lowercase hex without the "0x"
// prefix ("3e98"), or "" when the attribute is missing or malformed.
func intelDeviceID(deviceDir string) string {
	id, ok := strings.CutPrefix(strings.ToLower(sysfsField(filepath.Join(deviceDir, "device"))), "0x")
	if !ok || !isHex(id) {
		return ""
	}
	return id
}

// drmDriverName reports the kernel driver bound to a PCI device. The `driver`
// entry is a symlink into /sys/bus/pci/drivers, so the resolved directory name
// is the driver name; uevent's DRIVER= carries the same string and is the
// fallback for a sysfs view where the link cannot be resolved. "" means no
// driver is bound.
func drmDriverName(deviceDir string) string {
	link := filepath.Join(deviceDir, "driver")
	if resolved, err := filepath.EvalSymlinks(link); err == nil {
		return filepath.Base(resolved)
	}
	if target, err := os.Readlink(link); err == nil {
		return filepath.Base(target)
	}
	return ueventValue(readSysfs(filepath.Join(deviceDir, "uevent")), "DRIVER")
}

// intelModelName resolves a device id to a display name. An unknown id keeps
// the id in the name rather than being dropped, so a part released after
// intelModels was written still appears in the inventory and is still
// identifiable by anyone who can read an lspci line.
func intelModelName(deviceID string) string {
	if m, ok := intelModels[deviceID]; ok {
		return m.name
	}
	if deviceID == "" {
		return intelFallbackName
	}
	return intelFallbackName + " (device 0x" + deviceID + ")"
}

// intelDiscrete reports whether this card has dedicated VRAM. A listed part is
// taken at its word; an unlisted one is judged by its own numbers, because a
// driver only publishes mem_info_vram_total for a card that actually has local
// memory. Mirrors amdUnifiedPool's structure so the two vendors' verdicts are
// reached the same way.
func intelDiscrete(c intelCard) bool {
	if m, ok := intelModels[c.deviceID]; ok {
		return m.discrete
	}
	_, haveVRAM := drmSysfsUint(c.deviceDir, "mem_info_vram_total")
	return haveVRAM
}

// mergeIntelStats folds each Intel card's hwmon temperature into the
// snapshot's GPU map under its statsKey. In practice that means discrete Arc
// cards only: an integrated GPU registers no hwmon of its own (verified on the
// measured i915 host, whose device directory has no hwmon at all), so this
// adds nothing and allocates nothing there.
//
// It runs after applyGPUStats for the same reason mergeRockchipStats does: on
// a stale-preserve tick the snapshot's GPU map aliases the already-published
// previous map, so it must be cloned before anything is added. And like the
// accelerator and Rockchip samples it deliberately leaves GPUSampledAt
// untouched — a temperature is not a utilization sample, and i915 exposes no
// utilization at all, so an Intel reading must never advertise this host as
// having live GPU telemetry.
func mergeIntelStats(snap *statsSnapshot) {
	mergeIntelStatsFrom(drmClassDir, snap)
}

// mergeIntelStatsFrom is mergeIntelStats with the class root injected, so the
// merge itself is testable against a fake tree instead of whatever GPUs the
// test host happens to have.
func mergeIntelStatsFrom(drmRoot string, snap *statsSnapshot) {
	temps := intelTemperatures(drmRoot)
	if len(temps) == 0 {
		return
	}
	merged := make(map[string]gpuStat, len(snap.GPU)+len(temps))
	for k, v := range snap.GPU {
		merged[k] = v
	}
	for key, temp := range temps {
		stat := merged[key]
		stat.TemperatureC = temp
		merged[key] = stat
	}
	snap.GPU = merged
}

// intelTemperatures samples every Intel card under drmRoot that exposes a
// hwmon temperature, keyed by the same statsKey the inventory stamped on the
// matching GPUInfo. A card with no readable sensor is left out of the map
// entirely, so a transient sysfs failure keeps the row's previous value rather
// than publishing a zero over it.
func intelTemperatures(drmRoot string) map[string]uint32 {
	var temps map[string]uint32
	for _, c := range listIntelCards(drmRoot) {
		temp, ok := intelHwmonTempC(c.deviceDir)
		if !ok {
			continue
		}
		if temps == nil {
			temps = map[string]uint32{}
		}
		temps[c.statsKey] = temp
	}
	return temps
}

// intelHwmonTempC returns the card's temperature in whole degrees Celsius from
// the first hwmon under its device directory that publishes temp1_input
// (millidegrees). i915 and xe both register a single GPU sensor there on the
// parts that have one, so there is no label to disambiguate and no equivalent
// of amdgpu's edge/junction/mem trio to pick from.
func intelHwmonTempC(deviceDir string) (uint32, bool) {
	hwmonRoot := filepath.Join(deviceDir, "hwmon")
	for _, name := range sortedDirNames(hwmonRoot) {
		if temp, ok := parseMillidegrees(readSysfs(filepath.Join(hwmonRoot, name, "temp1_input"))); ok {
			return temp, true
		}
	}
	return 0, false
}
