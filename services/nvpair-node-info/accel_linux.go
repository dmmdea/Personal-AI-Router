// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"nvpair-shared/noderec"
)

// Linux inference-accelerator detection and sampling.
//
// The GPU inventory comes from the display/compute stack (nvidia-smi, ghw),
// which never sees an inference accelerator that is not a GPU: a Google Coral
// Edge TPU sits on PCIe as class "System peripheral" behind the gasket/apex
// driver and is only visible under /sys/class/apex. This file lists those
// devices in the same inventory (Kind = noderec.GPUKindAccelerator) so every
// client shows them, and derives the two dynamic numbers the driver supports:
//
//   - utilization: gasket keeps no busy counter, but every unit of work the
//     device completes raises host interrupts (interrupt_counts). We poll that
//     file every accelSampleInterval and call a sub-interval "busy" when the
//     counter moved; the reported figure is the busy fraction of the last
//     accelWindow sub-intervals. That is the definition nvidia-smi uses for
//     utilization.gpu ("percent of time a kernel was executing"), at 100 ms
//     rather than driver-level resolution.
//   - temperature: `temp` (millidegrees Celsius) on the same sysfs node.
//
// Nothing here counts toward GPUSampledAt / TelemetryValid: an accelerator
// cannot run PAIR's engines, so it must not make a GPU-less host look like it
// has fresh GPU telemetry, and noderec.MaxGPUUtilization skips these rows.

const (
	apexClassDir        = "/sys/class/apex"
	accelSampleInterval = 100 * time.Millisecond
	// accelWindow is the number of sub-intervals one reported figure covers
	// (10 x 100 ms = the collector's 1 s tick).
	accelWindow = 10
)

// apexProductNames maps a PCI "vendor:device" id pair to a display name.
// Unknown ids get a generic label carrying the ids rather than being dropped.
var apexProductNames = map[string]string{
	"1ac1:089a": "Google Coral Edge TPU",
}

// apexDevice is one /sys/class/apex/<name> entry.
type apexDevice struct {
	name string // e.g. apex_0
	dir  string // e.g. /sys/class/apex/apex_0
}

// accelStatsKey is the statsKey that joins a static accelerator row to its
// sampler's snapshot entry. Namespaced so it can never collide with an
// nvidia-smi UUID.
func accelStatsKey(dev string) string { return "apex:" + dev }

// listApexDevices enumerates /sys/class/apex sorted by name for a stable
// inventory order. A missing directory (no gasket driver loaded) is the normal
// no-accelerator case and returns nil silently.
func listApexDevices() []apexDevice {
	entries, err := os.ReadDir(apexClassDir)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Debug("apex class dir unreadable", "dir", apexClassDir, "err", err)
		}
		return nil
	}
	var devs []apexDevice
	for _, e := range entries {
		devs = append(devs, apexDevice{name: e.Name(), dir: filepath.Join(apexClassDir, e.Name())})
	}
	sort.Slice(devs, func(i, j int) bool { return devs[i].name < devs[j].name })
	return devs
}

// detectAccelerators returns one inventory row per apex device. The name is
// resolved from the PCI ids behind the class entry's `device` link; the
// dynamic fields are filled by buildResponse from the sampler keyed by
// accelStatsKey.
func detectAccelerators() []GPUInfo {
	var out []GPUInfo
	for _, dev := range listApexDevices() {
		vendor := readSysfs(filepath.Join(dev.dir, "device", "vendor"))
		device := readSysfs(filepath.Join(dev.dir, "device", "device"))
		out = append(out, apexRow(apexProductName(vendor, device), dev.name))
	}
	return out
}

// apexRow is one Edge TPU's inventory row. Its utilization is a real
// measurement (the interrupt-count sampler), so it carries no
// utilization_unavailable; but no engine PAIR runs can schedule work on an
// Edge TPU, so it is marked not inference-ready.
func apexRow(name, devName string) GPUInfo {
	return GPUInfo{
		Name:           name,
		Kind:           noderec.GPUKindAccelerator,
		InferenceReady: notInferenceReady(),
		statsKey:       accelStatsKey(devName),
	}
}

// apexProductName maps sysfs vendor/device strings ("0x1ac1", "0x089a") to a
// display name via apexProductNames.
func apexProductName(vendor, device string) string {
	v := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(vendor)), "0x")
	d := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(device)), "0x")
	if name, ok := apexProductNames[v+":"+d]; ok {
		return name
	}
	if v == "" || d == "" {
		return "Edge TPU (apex)"
	}
	return "Edge TPU (apex " + v + ":" + d + ")"
}

// readSysfs returns a sysfs attribute's contents, or "" when unreadable.
func readSysfs(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

// parseInterruptCounts sums every counter in gasket's interrupt_counts
// attribute ("0x00: 12 0x01: 0 ..."). Tokens ending in ':' are vector labels;
// every other token that parses as an unsigned integer is a count. ok is false
// when no count parsed at all (empty or foreign file).
func parseInterruptCounts(s string) (sum uint64, ok bool) {
	for _, f := range strings.Fields(s) {
		if strings.HasSuffix(f, ":") {
			continue
		}
		v, err := strconv.ParseUint(f, 10, 64)
		if err != nil {
			continue
		}
		sum += v
		ok = true
	}
	return sum, ok
}

// parseMillidegrees converts gasket's `temp` attribute (millidegrees Celsius,
// e.g. "51550") to whole degrees, rounded. Negative or unparseable input is
// reported as unavailable so the field drops from JSON.
func parseMillidegrees(s string) (uint32, bool) {
	v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || v < 0 {
		return 0, false
	}
	return uint32((v + 500) / 1000), true
}

// busyWindow is a ring of the most recent sub-interval busy flags.
type busyWindow struct {
	flags  [accelWindow]bool
	next   int
	filled int
}

func (w *busyWindow) push(busy bool) {
	w.flags[w.next] = busy
	w.next = (w.next + 1) % accelWindow
	if w.filled < accelWindow {
		w.filled++
	}
}

// percent is the busy fraction of the flags observed so far, 0..100. Before
// any observation it reports 0 (idle/unknown, matching the omitempty rule).
func (w *busyWindow) percent() uint32 {
	if w.filled == 0 {
		return 0
	}
	busy := 0
	for i := 0; i < w.filled; i++ {
		if w.flags[i] {
			busy++
		}
	}
	return uint32(math.Round(float64(busy) * 100 / float64(w.filled)))
}

// accelSampler polls one apex device in its own goroutine and publishes the
// latest gpuStat atomically; the collector's 1 s tick just reads it. The
// observe/stat pair holds the logic so it is unit-testable without sysfs.
type accelSampler struct {
	key        string
	countsPath string
	tempPath   string

	// Sampler-goroutine state; never touched by other goroutines.
	window   busyWindow
	prev     uint64
	havePrev bool
	temp     uint32

	latest atomic.Pointer[gpuStat]
	stop   chan struct{}
	done   chan struct{}
}

func newAccelSampler(dev apexDevice) *accelSampler {
	return &accelSampler{
		key:        accelStatsKey(dev.name),
		countsPath: filepath.Join(dev.dir, "interrupt_counts"),
		tempPath:   filepath.Join(dev.dir, "temp"),
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
	}
}

// startAccelSamplers spins up one sampler per device. Returns nil for no
// devices so callers can range over it unconditionally.
func startAccelSamplers(devs []apexDevice) []*accelSampler {
	var samplers []*accelSampler
	for _, dev := range devs {
		s := newAccelSampler(dev)
		go s.run()
		samplers = append(samplers, s)
	}
	return samplers
}

// observe folds one interrupt-counter reading into the busy window. The first
// successful reading only primes the baseline; a failed read (ok=false) is
// skipped rather than recorded as idle, so a transient sysfs error does not
// masquerade as an idle device.
func (s *accelSampler) observe(cur uint64, ok bool) {
	if !ok {
		return
	}
	if s.havePrev {
		s.window.push(cur != s.prev)
	}
	s.prev, s.havePrev = cur, true
}

func (s *accelSampler) stat() gpuStat {
	return gpuStat{UtilizationPct: s.window.percent(), TemperatureC: s.temp}
}

func (s *accelSampler) readCounts() (uint64, bool) {
	return parseInterruptCounts(readSysfs(s.countsPath))
}

func (s *accelSampler) readTemp() {
	if t, ok := parseMillidegrees(readSysfs(s.tempPath)); ok {
		s.temp = t
	}
}

func (s *accelSampler) run() {
	defer close(s.done)
	ticker := time.NewTicker(accelSampleInterval)
	defer ticker.Stop()
	s.observe(s.readCounts())
	s.readTemp()
	ticks := 0
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			s.observe(s.readCounts())
			ticks++
			if ticks%accelWindow == 0 {
				s.readTemp()
			}
			st := s.stat()
			s.latest.Store(&st)
		}
	}
}

// Latest returns the most recent published sample, or false before the first.
func (s *accelSampler) Latest() (gpuStat, bool) {
	p := s.latest.Load()
	if p == nil {
		return gpuStat{}, false
	}
	return *p, true
}

func (s *accelSampler) Stop() {
	close(s.stop)
	<-s.done
}
