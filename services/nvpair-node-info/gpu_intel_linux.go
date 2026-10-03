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
	"time"

	"nvpair-shared/gpunames"
	"nvpair-shared/noderec"
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
//   - No busy percentage attribute. i915 keeps its engine-busy counters in a
//     PMU exposed through perf_event_open, which is root-gated by default
//     (perf_event_paranoid), not in sysfs. There is no unprivileged sysfs
//     attribute equivalent to amdgpu's gpu_busy_percent. What there is, is
//     the RC6 idle residency counter (and xe's GT idle residency), from which
//     gpu_intel_busy_linux.go derives an estimate over time. Until that has a
//     delta to work with, and on a kernel that exposes neither counter, an
//     Intel row carries NO utilization_percent at all rather than a fabricated
//     0, and sets utilization_unavailable so a client can tell the absence from
//     an idle reading (utilization_percent is omitempty, so a measured 0 is
//     absent too) — "idle" and "we cannot tell" must never render the same.
//   - No temperature on an integrated GPU. The only sensor near it is the CPU
//     package sensor, which is the CPU's reading and is already published as
//     cpu.temperature_celsius; repeating it as the GPU's would be a lie about
//     a different piece of silicon. A discrete Arc card has its own hwmon, and
//     that one is read.
//
// Memory. An integrated Intel GPU has no dedicated VRAM: it allocates out of
// system DRAM, exactly like the Mali rows in gpu_rockchip_linux.go. Those rows
// carry the system-memory total as VramBytes and are stamped
// noderec.GPUMemoryPoolUnified, so a consumer can show the real ceiling and
// still say whose memory it is. A discrete card reports its own
// mem_info_vram_total when the driver exposes one; i915 does not expose it for
// an iGPU (verified on the measured host: the attribute is absent), and an
// unknown capacity stays 0, which omitempty drops from the wire.
//
// What an Intel row does NOT carry is a used figure. i915 and xe publish no
// unprivileged per-device allocation counter, and the host's own RAM usage is
// not a stand-in for one: the iGPU holds a framebuffer and whatever a compute
// context mapped, while /proc/meminfo counts every process on the box.
// Publishing the second as the first had a UHD Graphics 630 reading
// "VRAM 25.5 GB / 66 GB" on a host whose GPU was doing nothing. An omitted
// number renders as a shared-pool ceiling instead, which is what is known.

const (
	// intelPCIVendor is Intel's PCI vendor id as the sysfs attribute spells it.
	intelPCIVendor = "0x8086"

	// intelStatsKeyPrefix namespaces the statsKey so it can never collide with
	// an nvidia-smi UUID, an "amd:" row or an "apex:" accelerator key. Same
	// "<vendor>:<pci address>" shape as the amdgpu inventory, built by the same
	// helper (drmPCIStatsKey).
	intelStatsKeyPrefix = "intel:"
)

// intelDrivers are the kernel drivers that own a modern Intel GPU: i915 for
// everything up to and including Meteor Lake plus the Arc A-series, xe for
// Lunar Lake, Battlemage and later. A card bound to neither (vfio-pci for a
// passed-through GPU, or no driver at all) is skipped: it is not this host's
// GPU to report on, and none of the attributes below would be readable.
var intelDrivers = map[string]bool{"i915": true, "xe": true}

// The Intel id -> name table lives in nvpair-shared/gpunames, because the
// Windows inventory has to reach exactly the same answer from a DXGI
// adapter's VendorID/DeviceID pair. This file only turns a sysfs device id
// into the arguments that package takes.

// intelCard is one enumerated Intel adapter: where its attributes live, what
// it is, and the key its samples are published under.
type intelCard struct {
	card      string // DRM node name, e.g. "card1"
	index     int    // the N of card<N>, for a stable numeric inventory order
	cardDir   string // <drmRoot>/card<N>, where i915 keeps gt/ and power/
	deviceDir string // <drmRoot>/card<N>/device
	deviceID  string // PCI device id, lowercase hex without "0x", e.g. "3e98"
	driver    string // bound kernel driver, "i915" or "xe"

	// discrete marks a card with its own VRAM. An integrated GPU allocates out
	// of system DRAM and its row is published as a unified pool instead.
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
// in card-number order. A temperature (on an iGPU) is left absent on purpose —
// see the ceilings at the top of this file — so the dynamic fields an Intel row
// can gain are a discrete Arc card's temperature and the utilization derived
// from the idle-residency counter, both merged in by mergeIntelStats.
func detectIntelGPUs(drmRoot string) []GPUInfo {
	var out []GPUInfo
	for _, c := range listIntelCards(drmRoot) {
		row := GPUInfo{
			Name:     intelModelName(c.deviceID),
			statsKey: c.statsKey,
		}
		if len(intelIdleCounters(c)) > 0 {
			// The driver publishes an idle-residency counter, so utilization
			// is derived from it (gpu_intel_busy_linux.go). buildResponseAt
			// publishes the row as UtilizationUnavailable until the sampler
			// has a reading, so an absent figure never reads as idle.
			row.utilizationNeedsSample = true
		} else {
			// No counter this service can read (see the top of this file), so
			// the row says so rather than leaving an absent utilization for a
			// client to read as idle.
			row.UtilizationUnavailable = true
		}
		if c.discrete {
			// Only if the driver actually publishes it: i915 does not expose
			// this attribute for a discrete card on every kernel, and an
			// invented capacity is worse than an omitted one.
			row.VramBytes, _ = drmSysfsUint(c.deviceDir, "mem_info_vram_total")
		} else {
			// The pool is the host's and nothing here measures the GPU's share
			// of it, so the row publishes the ceiling and no usage at all.
			row.VramBytes = systemMemTotal()
			row.MemoryPool = noderec.GPUMemoryPoolUnified
			// And no engine PAIR runs on Linux drives an Intel iGPU, so a
			// client must not present it as where the node's models run.
			row.InferenceReady = notInferenceReady()
		}
		slog.Debug("Intel GPU detected",
			"name", row.Name, "stats_key", row.statsKey, "driver", c.driver,
			"discrete", c.discrete, "vram_bytes", row.VramBytes,
			"memory_pool", row.MemoryPool)
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
			cardDir:   filepath.Join(drmRoot, e.Name()),
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

// intelModelName resolves a sysfs device id to a display name. An unknown id
// keeps the id in the name rather than being dropped, so a part released after
// the shared table was written still appears in the inventory and is still
// identifiable by anyone who can read an lspci line.
func intelModelName(deviceID string) string {
	id, ok := gpunames.ParseHexID(deviceID)
	return gpunames.IntelName(id, ok)
}

// intelDiscrete reports whether this card has dedicated VRAM. A listed part is
// taken at its word; an unlisted one is judged by its own numbers, because a
// driver only publishes mem_info_vram_total for a card that actually has local
// memory. Mirrors amdUnifiedPool's structure so the two vendors' verdicts are
// reached the same way.
func intelDiscrete(c intelCard) bool {
	if id, ok := gpunames.ParseHexID(c.deviceID); ok {
		if discrete, known := gpunames.IntelDiscrete(id); known {
			return discrete
		}
	}
	_, haveVRAM := drmSysfsUint(c.deviceDir, "mem_info_vram_total")
	return haveVRAM
}

// mergeIntelStats folds each Intel card's samples into the snapshot's GPU map
// under its statsKey: the hwmon temperature of a discrete Arc card (an
// integrated GPU registers no hwmon of its own — verified on the measured i915
// host, whose device directory has no hwmon at all), and the utilization the
// idle-residency sampler derived for any card that has a counter
// (gpu_intel_busy_linux.go).
//
// It runs after applyGPUStats for the same reason mergeRockchipStats does: on
// a stale-preserve tick the snapshot's GPU map aliases the already-published
// previous map, so it must be cloned before anything is added. And like the
// accelerator and Rockchip samples it deliberately leaves GPUSampledAt
// untouched — neither a temperature nor an integrated GPU's utilization is
// telemetry of a device PAIR's engines run on (the row is inference_ready:false),
// so an Intel reading must never advertise this host as having live GPU
// telemetry.
func mergeIntelStats(util *intelUtilSampler, snap *statsSnapshot) {
	mergeIntelSamples(drmClassDir, util, time.Now(), snap)
}

// mergeIntelStatsFrom is mergeIntelStats for the temperature alone with the
// class root injected, so the merge itself is testable against a fake tree
// instead of whatever GPUs the test host happens to have.
func mergeIntelStatsFrom(drmRoot string, snap *statsSnapshot) {
	mergeIntelSamples(drmRoot, nil, time.Time{}, snap)
}

// mergeIntelSamples is the merge with every input injected: the class root, the
// utilization sampler (nil skips utilization) and the sample time.
//
// A card whose sampler has no reading this tick publishes none. When the map
// being merged into already carries a known utilization for that card — which
// it can, because a stale-preserve tick aliases the previous snapshot's map —
// that entry is cleared, so a reading that has gone away is not frozen on the
// wire by the stale-preserve path. Where there is nothing to add and nothing to
// clear the map is left exactly as it was and nothing is allocated, which is
// the ordinary case on a host whose iGPU has no counter yet.
func mergeIntelSamples(drmRoot string, util *intelUtilSampler, now time.Time, snap *statsSnapshot) {
	cards := listIntelCards(drmRoot)
	if len(cards) == 0 {
		return
	}
	temps := intelTemperaturesOf(cards)
	var pcts map[string]uint32
	if util != nil {
		pcts = util.sample(cards, now)
	}
	var merged map[string]gpuStat
	edit := func(key string, fn func(*gpuStat)) {
		if merged == nil {
			merged = make(map[string]gpuStat, len(snap.GPU)+len(cards))
			for k, v := range snap.GPU {
				merged[k] = v
			}
		}
		stat := merged[key]
		fn(&stat)
		merged[key] = stat
	}
	for _, c := range cards {
		if temp, ok := temps[c.statsKey]; ok {
			edit(c.statsKey, func(s *gpuStat) { s.TemperatureC = temp })
		}
		if util == nil {
			continue
		}
		if pct, ok := pcts[c.statsKey]; ok {
			edit(c.statsKey, func(s *gpuStat) {
				s.UtilizationPct = pct
				s.UtilizationKnown = true
			})
		} else if snap.GPU[c.statsKey].UtilizationKnown {
			edit(c.statsKey, func(s *gpuStat) {
				s.UtilizationPct = 0
				s.UtilizationKnown = false
			})
		}
	}
	if merged != nil {
		snap.GPU = merged
	}
}

// intelTemperatures samples every Intel card under drmRoot that exposes a
// hwmon temperature, keyed by the same statsKey the inventory stamped on the
// matching GPUInfo. A card with no readable sensor is left out of the map
// entirely, so a transient sysfs failure keeps the row's previous value rather
// than publishing a zero over it.
func intelTemperatures(drmRoot string) map[string]uint32 {
	return intelTemperaturesOf(listIntelCards(drmRoot))
}

func intelTemperaturesOf(cards []intelCard) map[string]uint32 {
	var temps map[string]uint32
	for _, c := range cards {
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
