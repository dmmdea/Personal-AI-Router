// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"errors"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"nvpair-shared/noderec"
)

// Rockchip RKNPU (RK35xx neural processing unit) inventory and sampling.
//
// The NPU is listed in the same inventory as the GPUs with
// Kind = noderec.GPUKindAccelerator, exactly like a Coral Edge TPU: it is an
// inference accelerator PAIR's engines cannot run on, so it must never count
// toward GPU pressure or make the host look like it has live GPU telemetry.
// Unlike the Coral it is not a PCIe device behind the gasket/apex class — it is
// a platform device, found through its devfreq entry (uevent DRIVER=RKNPU).
//
// Its two dynamic numbers come from different places:
//
//   - utilization: NOT the devfreq load. The RKNPU devfreq node reports a load
//     pinned at 100 % while the NPU is completely idle (measured on RK3588S,
//     vendor kernel 6.1), so using it would report a permanently saturated
//     accelerator. The driver's real per-core counter lives in debugfs:
//     <debugfs>/rknpu/load, "NPU load:  Core0:  0%, Core1:  0%, Core2:  0%,".
//     The reported figure is the mean across cores, which is what "the NPU is
//     N % busy" means for a multi-core engine that schedules work per core.
//   - temperature: the thermal zone typed npu-thermal (millidegrees Celsius).
//
// debugfs is root-only by default (0700 on the mount point), so an unprivileged
// service reads nothing. That is a deployment choice, not a failure: the row
// stays in the inventory with its temperature, utilization is omitted, and one
// Info line names the file and the remount that would make it readable. The
// service never attempts the remount itself.

const (
	rknpuDriver      = "RKNPU"
	rknpuThermalZone = "npu-thermal"
	rknpuStatsPrefix = "rknpu:"
	rknpuDebugfsName = "rknpu"
	// rknpuRemountHint is the one-time operator hint printed when the load
	// counter is unreadable. debugfs mode 755 exposes only what its owners
	// already publish and is the standard way to let a service read it.
	rknpuRemountHint  = "mount -o remount,mode=755 /sys/kernel/debug (as root, e.g. from a boot unit) to report NPU utilization"
	rknpuFallbackName = "Rockchip NPU"
)

// rknpuSoCCores maps an RKNPU device-tree compatible string to the SoC's NPU
// core count. It is only consulted when the debugfs counter is unreadable —
// when it can be read, the core count is whatever the driver prints, which is
// authoritative even on a SoC this table does not know.
var rknpuSoCCores = map[string]int{
	"rockchip,rk3588-rknpu": 3,
	"rockchip,rk3576-rknpu": 2,
	"rockchip,rk3568-rknpu": 1,
	"rockchip,rk3566-rknpu": 1,
	"rockchip,rk3562-rknpu": 1,
}

// rknpuCorePattern matches one "Core<N>: <pct>%" field of the debugfs load
// line, whatever the spacing the driver version uses.
var rknpuCorePattern = regexp.MustCompile(`(?i)core\s*\d+\s*:\s*(\d+)\s*%`)

// rknpuDevice is the NPU as found in sysfs plus the files its sampler reads.
// warnOnce keeps the unreadable-debugfs notice to a single line for the life of
// the process — the sampler retries every tick and must not flood the log.
type rknpuDevice struct {
	node       string // devfreq/platform node name, the statsKey suffix
	compatible string // first device-tree compatible string, e.g. rockchip,rk3588-rknpu
	loadPath   string // <debugfs>/rknpu/load
	tempPath   string // npu-thermal zone temp; "" when absent

	warnOnce sync.Once
}

// detectRockchipNPU returns the RKNPU accelerator row and starts its sampler,
// or nil on a host without the driver. Called from detectRockchipDevices.
func detectRockchipNPU() []GPUInfo {
	dev, ok := findRKNPUDevice(systemRockchipRoots())
	if !ok {
		return nil
	}
	row := rknpuRow(dev, dev.cores(), systemMemTotal())
	slog.Debug("RKNPU detected",
		"name", row.Name, "stats_key", row.statsKey,
		"compatible", dev.compatible, "load", dev.loadPath, "thermal", dev.tempPath)
	if dev.tempPath == "" {
		slog.Info("no RKNPU thermal zone on this host; the NPU temperature will be omitted",
			"zone_type", rknpuThermalZone)
	}
	startRockchipSampler(newRockchipSampler(row.statsKey, dev.readUtilization, dev.tempPath))
	return []GPUInfo{row}
}

// rknpuRow builds the inventory row. Memory is unified (the NPU works out of
// system DRAM), so the row carries the system-memory total stamped as a shared
// pool. It carries no used figure, for the reason maliRow gives: the RKNPU
// driver reports no allocation of its own, and the host's RAM usage is not the
// NPU's — that substitution is what had an 8 GB board claiming the NPU held
// 1.1 GB of memory it had never asked for.
func rknpuRow(dev *rknpuDevice, cores int, memTotal uint64) GPUInfo {
	return GPUInfo{
		Name:       rknpuProductName(dev.compatible, cores),
		Kind:       noderec.GPUKindAccelerator,
		VramBytes:  memTotal,
		statsKey:   rknpuStatsPrefix + dev.node,
		MemoryPool: noderec.GPUMemoryPoolUnified,
	}
}

// findRKNPUDevice scans the devfreq class for the entry whose uevent names the
// RKNPU driver. A host without the driver returns false silently.
//
// The uevent that carries DRIVER= and the device-tree keys is the platform
// device's, behind the class node's "device" link — the class node's own uevent
// is empty on the vendor kernel. It is still read as a fallback, for a kernel
// that populates it.
func findRKNPUDevice(r rockchipRoots) (*rknpuDevice, bool) {
	for _, name := range sortedDirNames(r.devfreq) {
		uevent := readSysfs(filepath.Join(r.devfreq, name, "device", "uevent"))
		if !strings.EqualFold(ueventValue(uevent, "DRIVER"), rknpuDriver) {
			uevent = readSysfs(filepath.Join(r.devfreq, name, "uevent"))
		}
		if !strings.EqualFold(ueventValue(uevent, "DRIVER"), rknpuDriver) {
			continue
		}
		return &rknpuDevice{
			node:       name,
			compatible: ueventValue(uevent, "OF_COMPATIBLE_0"),
			loadPath:   filepath.Join(r.debugfs, rknpuDebugfsName, "load"),
			tempPath:   findThermalZone(r.thermal, rknpuThermalZone),
		}, true
	}
	return nil, false
}

// cores reports how many NPU cores to name in the inventory: what the driver
// prints when debugfs is readable, otherwise the SoC's known count, otherwise
// 0 (the name then carries no core count rather than a guessed one).
func (d *rknpuDevice) cores() int {
	if _, cores, ok := d.readLoad(); ok {
		return cores
	}
	return rknpuSoCCores[d.compatible]
}

// rknpuProductName builds "Rockchip RK3588 NPU (3 cores)" from the device-tree
// compatible string and the core count, degrading to a generic name when the
// compatible string is missing or unparseable.
func rknpuProductName(compatible string, cores int) string {
	_, soc := splitCompatible(compatible)
	if soc == "" {
		return rknpuFallbackName
	}
	name := "Rockchip " + soc + " NPU"
	switch {
	case cores == 1:
		name += " (1 core)"
	case cores > 1:
		name += " (" + strconv.Itoa(cores) + " cores)"
	}
	return name
}

// readUtilization is the sampler's source: the mean across NPU cores, or false
// when the counter is unreadable (which keeps the row's temperature-only
// reporting rather than claiming an idle NPU).
func (d *rknpuDevice) readUtilization() (uint32, bool) {
	mean, _, ok := d.readLoad()
	return mean, ok
}

// readLoad reads and parses the debugfs load counter. A permission error is the
// expected unprivileged case and is reported once, with the file and the
// remount that fixes it; any other error is a debug-level detail.
func (d *rknpuDevice) readLoad() (mean uint32, cores int, ok bool) {
	data, err := os.ReadFile(d.loadPath)
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			d.warnOnce.Do(func() {
				slog.Info("RKNPU load counter unreadable; reporting NPU temperature only",
					"path", d.loadPath, "hint", rknpuRemountHint)
			})
		} else {
			slog.Debug("read RKNPU load failed", "path", d.loadPath, "err", err)
		}
		return 0, 0, false
	}
	return parseRKNPULoad(string(data))
}

// parseRKNPULoad decodes the RKNPU debugfs load line
//
//	NPU load:  Core0:  0%, Core1:  0%, Core2:  0%,
//
// into the mean busy percentage across the cores it lists and their count.
// Every core is clamped to 100 before averaging so one bogus field cannot push
// the reported figure past 100. ok is false when the text lists no core at all
// (an empty file, or a driver version whose format changed), which callers
// treat as "no reading" rather than as an idle NPU.
func parseRKNPULoad(s string) (mean uint32, cores int, ok bool) {
	matches := rknpuCorePattern.FindAllStringSubmatch(s, -1)
	if len(matches) == 0 {
		return 0, 0, false
	}
	var sum float64
	for _, m := range matches {
		pct, err := strconv.ParseUint(m[1], 10, 32)
		if err != nil || pct > 100 {
			pct = 100
		}
		sum += float64(pct)
	}
	return uint32(math.Round(sum / float64(len(matches)))), len(matches), true
}
