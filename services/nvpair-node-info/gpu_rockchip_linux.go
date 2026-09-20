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
	"sync/atomic"
	"time"
)

// Rockchip RK35xx integrated-GPU (Arm Mali) inventory and sampling, plus the
// sampler plumbing the RKNPU accelerator (accel_rockchip_linux.go) shares.
//
// Neither Linux GPU detector sees this GPU: there is no nvidia-smi, and ghw
// enumerates PCI display adapters while a Mali GPU is a platform device. The
// kernel driver publishes it under <miscClassDir>/mali0, whose "device" link is
// the platform node, and the two dynamic numbers come from two other class
// trees:
//
//   - utilization: the devfreq node's "load", formatted "<busy%>@<freq>Hz".
//     Kernels that do not expose it fall back to the driver's own
//     "utilisation" attribute (0..100) on the device node.
//   - temperature: the thermal zone typed gpu-thermal (millidegrees Celsius),
//     read through the same helper the CPU package sensor uses.
//
// Memory is unified — the GPU has no dedicated VRAM, it shares system DRAM — so
// the row is marked usesSystemMemoryUsage and carries the system-memory total
// as VramBytes, exactly like the nvidia UMA branch in gpu_linux.go.
//
// Sampling runs in one goroutine per device rather than inside the collector
// tick: sysfs reads are cheap, but a wedged driver node must never be able to
// delay the snapshot every HTTP handler reads.

const (
	// miscClassDir holds the Mali driver's character device; the GPU itself is
	// the platform device behind <miscClassDir>/<maliMiscName>/device.
	miscClassDir = "/sys/class/misc"
	// devfreqClassDir holds one directory per devfreq-managed device. Both the
	// Mali GPU and the RKNPU appear here under their platform node name, which
	// is what their samples are keyed by.
	devfreqClassDir = "/sys/class/devfreq"
	// debugfsDir is the kernel debug filesystem mount point, where the RKNPU
	// driver publishes its only usable load counter.
	debugfsDir = "/sys/kernel/debug"

	maliMiscName     = "mali0"
	maliThermalZone  = "gpu-thermal"
	maliStatsPrefix  = "mali:"
	maliFallbackName = "Arm Mali GPU"

	// rockchipSampleInterval matches the collector's 1 s tick. Both sources are
	// instantaneous readings, so sampling faster would only add jitter.
	rockchipSampleInterval = time.Second
)

// rockchipRoots are the class directories the Rockchip detectors read. Held as
// a struct so tests can point every lookup at a fake tree.
type rockchipRoots struct {
	misc    string // /sys/class/misc
	devfreq string // /sys/class/devfreq
	thermal string // /sys/class/thermal
	debugfs string // /sys/kernel/debug
}

func systemRockchipRoots() rockchipRoots {
	return rockchipRoots{
		misc:    miscClassDir,
		devfreq: devfreqClassDir,
		thermal: thermalClassDir,
		debugfs: debugfsDir,
	}
}

// systemMemTotal caches the system-memory total shared by every unified-memory
// row on this host. Both detectors need it and neither should pay for a second
// ghw introspection.
var systemMemTotal = sync.OnceValue(detectMemoryTotal)

// maliDevice is the Mali GPU as found in sysfs: a display name, the devfreq
// node name that becomes its statsKey, and the files its sampler reads.
type maliDevice struct {
	name     string // e.g. "Arm Mali-G610 MP4"
	node     string // devfreq/platform node name; "" when the kernel exposes none
	loadPath string // devfreq load attribute; "" when absent
	utilPath string // driver utilisation attribute (fallback source)
	tempPath string // gpu-thermal zone temp; "" when absent
}

// statsKey is the join key between this row and its sampler's snapshot entry.
// Namespaced so it can never collide with an nvidia-smi UUID or an apex key.
func (d maliDevice) statsKey() string {
	if d.node == "" {
		return maliStatsPrefix + maliMiscName
	}
	return maliStatsPrefix + d.node
}

// detectRockchipDevices returns every Rockchip inference-capable device on this
// host: the Mali GPU row plus the RKNPU accelerator row. It is the single hook
// gpu_linux.go calls, because detectAccelerators() covers only the gasket/apex
// class and the NPU has to reach the inventory through some detector. The NPU
// row carries Kind = noderec.GPUKindAccelerator, so every consumer still treats
// it as an accelerator rather than as a GPU.
func detectRockchipDevices() []GPUInfo {
	return append(detectRockchipGPUs(), detectRockchipNPU()...)
}

// detectRockchipGPUs returns the Mali GPU row and starts its sampler, or nil on
// a host with no Mali driver (every non-Rockchip machine).
func detectRockchipGPUs() []GPUInfo {
	dev, ok := findMaliDevice(systemRockchipRoots())
	if !ok {
		return nil
	}
	row := maliRow(dev, systemMemTotal())
	slog.Debug("Mali GPU detected",
		"name", row.Name, "stats_key", row.statsKey,
		"load", dev.loadPath, "utilisation", dev.utilPath, "thermal", dev.tempPath)
	if dev.tempPath == "" {
		slog.Info("no Mali thermal zone on this host; the GPU temperature will be omitted",
			"zone_type", maliThermalZone)
	}
	startRockchipSampler(newRockchipSampler(row.statsKey, dev.readUtilization, dev.tempPath))
	return []GPUInfo{row}
}

// maliRow builds the inventory row. VramBytes is the system-memory total and
// usesSystemMemoryUsage makes response assembly fill VramUsedBytes from the
// system-memory sample — the same contract the nvidia UMA rows use.
func maliRow(dev maliDevice, memTotal uint64) GPUInfo {
	return GPUInfo{
		Name:                  dev.name,
		VramBytes:             memTotal,
		statsKey:              dev.statsKey(),
		usesSystemMemoryUsage: true,
	}
}

// findMaliDevice locates the Mali platform device and the files its sampler
// reads. A missing misc node is the normal "no Mali GPU here" case and returns
// false silently.
func findMaliDevice(r rockchipRoots) (maliDevice, bool) {
	dir := filepath.Join(r.misc, maliMiscName, "device")
	if _, err := os.Stat(dir); err != nil {
		if !os.IsNotExist(err) {
			slog.Debug("Mali device node unreadable", "dir", dir, "err", err)
		}
		return maliDevice{}, false
	}
	dev := maliDevice{
		name:     maliProductName(readSysfs(filepath.Join(dir, "gpuinfo"))),
		node:     maliDevfreqNode(dir, r.devfreq),
		utilPath: filepath.Join(dir, "utilisation"),
		tempPath: findThermalZone(r.thermal, maliThermalZone),
	}
	if dev.node != "" {
		dev.loadPath = firstExistingFile(
			filepath.Join(dir, "devfreq", dev.node, "load"),
			filepath.Join(r.devfreq, dev.node, "load"),
		)
	}
	return dev, true
}

// maliDevfreqNode resolves the devfreq node name that identifies this GPU
// ("<address>.gpu" on every RK35xx device tree). The device's own devfreq
// directory is authoritative; when a kernel does not link it there, the devfreq
// class is scanned for the GPU node instead. "" means the kernel manages no
// devfreq for the GPU, in which case the sampler falls back to the driver's
// utilisation attribute and the row keys itself by the misc device name.
func maliDevfreqNode(deviceDir, devfreqRoot string) string {
	if names := sortedDirNames(filepath.Join(deviceDir, "devfreq")); len(names) > 0 {
		return names[0]
	}
	for _, name := range sortedDirNames(devfreqRoot) {
		if strings.HasSuffix(name, ".gpu") {
			return name
		}
	}
	return ""
}

// maliProductName derives the display name from the driver's gpuinfo attribute,
// e.g. "Mali-G610 4 cores r0p0 0xA867" -> "Arm Mali-G610 MP4". The MP suffix is
// the vendor's own shorthand for the shader-core count, so the name matches how
// the part is marketed. Anything unrecognizable degrades to a generic name
// rather than surfacing raw driver text.
func maliProductName(gpuinfo string) string {
	fields := strings.Fields(gpuinfo)
	if len(fields) == 0 || !strings.HasPrefix(strings.ToLower(fields[0]), "mali-") {
		return maliFallbackName
	}
	name := "Arm " + fields[0]
	if len(fields) >= 3 && strings.HasPrefix(strings.ToLower(fields[2]), "core") {
		if cores, err := strconv.Atoi(fields[1]); err == nil && cores > 0 {
			name += " MP" + strconv.Itoa(cores)
		}
	}
	return name
}

// readUtilization reports the GPU busy percentage: the devfreq load first, the
// driver's own utilisation attribute when the kernel exposes no devfreq load.
// false means neither source was readable this tick, which the sampler treats
// as "keep the previous value" rather than as idle.
func (d maliDevice) readUtilization() (uint32, bool) {
	if d.loadPath != "" {
		if pct, ok := parseDevfreqLoad(readSysfs(d.loadPath)); ok {
			return pct, true
		}
	}
	if d.utilPath == "" {
		return 0, false
	}
	return parsePercent(readSysfs(d.utilPath))
}

// parseDevfreqLoad reads the devfreq load attribute, "<busy%>@<freq>Hz" (e.g.
// "37@600000000Hz"). The frequency half is deliberately dropped: it describes
// the operating point, not the load. A bare number is accepted for kernels that
// print the percentage alone.
func parseDevfreqLoad(s string) (uint32, bool) {
	s = strings.TrimSpace(s)
	if at := strings.IndexByte(s, '@'); at >= 0 {
		s = s[:at]
	}
	return parsePercent(s)
}

// parsePercent parses a whole-percent sysfs attribute, clamped to 100 so a
// driver quirk can never surface an impossible figure.
func parsePercent(s string) (uint32, bool) {
	v, err := strconv.ParseUint(strings.TrimSpace(s), 10, 32)
	if err != nil {
		return 0, false
	}
	if v > 100 {
		v = 100
	}
	return uint32(v), true
}

// sortedDirNames lists a directory's entries by name, or nil when it does not
// exist. Sorted so device ordering is stable across boots.
func sortedDirNames(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// firstExistingFile returns the first path that exists, or "".
func firstExistingFile(paths ...string) string {
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// rockchipSampler polls one Rockchip device (Mali GPU or RKNPU) in its own
// goroutine and publishes the latest gpuStat atomically; the collector's tick
// only reads the published pointer. readUtil is the device-specific source; a
// failed read keeps the previous value rather than reporting a spurious 0, so a
// transient sysfs error cannot masquerade as an idle device.
type rockchipSampler struct {
	key      string
	readUtil func() (uint32, bool)
	tempPath string

	// Sampler-goroutine state; never touched by other goroutines.
	util uint32
	temp uint32

	latest atomic.Pointer[gpuStat]
	stop   chan struct{}
	done   chan struct{}
}

func newRockchipSampler(key string, readUtil func() (uint32, bool), tempPath string) *rockchipSampler {
	return &rockchipSampler{
		key:      key,
		readUtil: readUtil,
		tempPath: tempPath,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// sample takes one reading from both sources and publishes it.
func (s *rockchipSampler) sample() {
	if util, ok := s.readUtil(); ok {
		s.util = util
	}
	if s.tempPath != "" {
		if temp, ok := parseMillidegrees(readSysfs(s.tempPath)); ok {
			s.temp = temp
		}
	}
	st := gpuStat{UtilizationPct: s.util, TemperatureC: s.temp}
	s.latest.Store(&st)
}

func (s *rockchipSampler) run() {
	defer close(s.done)
	ticker := time.NewTicker(rockchipSampleInterval)
	defer ticker.Stop()
	s.sample()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			s.sample()
		}
	}
}

// Latest returns the most recent published sample, or false before the first.
func (s *rockchipSampler) Latest() (gpuStat, bool) {
	p := s.latest.Load()
	if p == nil {
		return gpuStat{}, false
	}
	return *p, true
}

func (s *rockchipSampler) Stop() {
	close(s.stop)
	<-s.done
}

// rockchipSamplerSet is the process-wide set of running Rockchip samplers.
// Detection registers one per device found (both detectors run once, at
// startup) and the collector tick folds their samples in through
// mergeRockchipStats. They live exactly as long as the inventory rows they
// feed — the process — so the service needs no stop path; stopRockchipSamplers
// exists so a test can start one without leaking a goroutine.
var rockchipSamplerSet struct {
	mu   sync.Mutex
	list []*rockchipSampler
}

func startRockchipSampler(s *rockchipSampler) {
	rockchipSamplerSet.mu.Lock()
	rockchipSamplerSet.list = append(rockchipSamplerSet.list, s)
	rockchipSamplerSet.mu.Unlock()
	go s.run()
}

func runningRockchipSamplers() []*rockchipSampler {
	rockchipSamplerSet.mu.Lock()
	defer rockchipSamplerSet.mu.Unlock()
	return rockchipSamplerSet.list
}

// stopRockchipSamplers stops and forgets every registered sampler.
func stopRockchipSamplers() {
	rockchipSamplerSet.mu.Lock()
	list := rockchipSamplerSet.list
	rockchipSamplerSet.list = nil
	rockchipSamplerSet.mu.Unlock()
	for _, s := range list {
		s.Stop()
	}
}

// mergeRockchipStats folds every Rockchip sampler's latest sample into the
// snapshot under its statsKey. It is called once per tick from decodeSnapshot,
// after applyGPUStats: on a stale-preserve tick the snapshot's GPU map aliases
// the already-published previous map, so it is cloned before anything is added.
// Like the accelerator samplers these samples deliberately leave GPUSampledAt
// untouched — PAIR's engines run on neither a Mali GPU nor an NPU, so a fresh
// sample here must not advertise this host as having live GPU telemetry.
func mergeRockchipStats(snap *statsSnapshot) {
	list := runningRockchipSamplers()
	if len(list) == 0 {
		return
	}
	merged := make(map[string]gpuStat, len(snap.GPU)+len(list))
	for k, v := range snap.GPU {
		merged[k] = v
	}
	for _, s := range list {
		if st, ok := s.Latest(); ok {
			merged[s.key] = st
		}
	}
	snap.GPU = merged
}
