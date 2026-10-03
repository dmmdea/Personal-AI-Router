// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Intel GPU utilization from the render-C-state (RC6) idle residency counter.
//
// i915 keeps its engine-busy counters in a PMU reached through perf_event_open,
// which perf_event_paranoid gates behind root, so a service running as the
// desktop user cannot use it (and neither can intel_gpu_top). What the driver
// does publish in plain sysfs, world-readable, is how long the graphics
// hardware has spent power-gated in RC6, the deepest render idle state, as a
// cumulative millisecond counter. The GPU is in RC6 exactly when it has
// nothing to do, so over a window:
//
//	busy % = 100 * (1 - dRC6_ms / dWall_ms)
//
// The same idea in the xe driver is the GT idle residency under gtidle/.
//
// This is an estimate and is documented as one: "not in RC6" is slightly wider
// than "an engine is executing" because the hardware waits a short hysteresis
// before it power-gates after the last batch. It is the closest unprivileged
// signal there is, it is 0 on an idle machine (measured: the counter advanced
// ~1007 ms over 1 s of wall time on an idle Coffee Lake UHD 630) and it climbs
// with real load, which is what a usage line has to do.
//
// Where the counter lives, per driver (a path is a source only if it reads):
//
//	i915, current kernels   /sys/class/drm/card<N>/gt/gt<M>/rc6_residency_ms
//	i915, older kernels     /sys/class/drm/card<N>/power/rc6_residency_ms
//	xe                      /sys/class/drm/card<N>/device/tile<T>/gt<M>/gtidle/idle_residency_ms
//
// The i915 paths are under the DRM card directory, the xe one under the PCI
// device directory. The gt<M> layout wins over the legacy file when both
// exist, because on a multi-GT part the legacy file covers GT0 only.
//
// Several GTs. A newer part has more than one graphics tile or GT (a render
// GT plus a media GT on Meteor Lake and later). Each GT has its own counter and
// its own busy fraction; the row publishes the MAX over them, the same rule the
// Windows collector uses across engine types (aggregateUtilization): the
// number a person recognises as "the GPU is busy" is the busiest part of it,
// and a media GT that is idle while the render GT is saturated must not drag
// the figure down. Averaging would hide a saturated GT, and summing idle
// residency across GTs is not a fraction at all (two GTs each idle for the
// whole window sum to 200 % of it).
//
// Sampling. A busy figure is a derivative, so the sampler keeps the previous
// counter per GT and divides by the wall time that actually elapsed between the
// two reads (monotonic clock), never by the tick constant, so a late tick
// cannot skew it. It runs inline on the collector's 1 s tick like the amdgpu
// reader, owned by the single ticker goroutine, and the collector primes it at
// startup (the way it primes its CPU baseline), so the first tick already has a
// delta. Without a previous sample there is no reading, and "no reading" is
// published as no utilization plus utilization_unavailable, never as 0.

// intelUtilHoldTicks is how many consecutive ticks without a usable reading the
// sampler publishes the last good value for before it says "unavailable"
// again. Same rule and same figure as rockchipUtilHoldTicks: one skipped sample
// (a counter reset, a transient sysfs error) must not flip the row between a
// number and a dash, but a counter that has gone away must not freeze the last
// value forever.
const intelUtilHoldTicks = 3

// intelIdleSample is one reading of one GT's cumulative idle residency.
type intelIdleSample struct {
	ms uint64    // cumulative milliseconds idle (RC6 / GT idle)
	at time.Time // when it was read; carries a monotonic reading
}

// intelIdleCounter is one GT's counter file and its identity within the card.
type intelIdleCounter struct {
	id   string // stable per-GT key, e.g. "gt0" or "tile0/gt0"
	path string
}

// intelIdleCounters lists the idle-residency counter files of a card, one per
// GT, in a stable order. Empty when the driver exposes none (an unsupported
// driver, a kernel without the attribute, a sysfs view that hides it).
func intelIdleCounters(c intelCard) []intelIdleCounter {
	switch c.driver {
	case "xe":
		return xeIdleCounters(c.deviceDir)
	default:
		return i915IdleCounters(c.cardDir)
	}
}

// i915IdleCounters finds <card>/gt/gt<M>/rc6_residency_ms for every GT, falling
// back to the single legacy <card>/power/rc6_residency_ms. A file only counts
// when it parses, so a path that exists but cannot be read is not a source.
func i915IdleCounters(cardDir string) []intelIdleCounter {
	var out []intelIdleCounter
	gtRoot := filepath.Join(cardDir, "gt")
	for _, name := range sortedDirNames(gtRoot) {
		if !isGTDirName(name, "gt") {
			continue
		}
		path := filepath.Join(gtRoot, name, "rc6_residency_ms")
		if _, ok := parseResidencyMS(readSysfs(path)); ok {
			out = append(out, intelIdleCounter{id: name, path: path})
		}
	}
	if len(out) > 0 {
		return out
	}
	path := filepath.Join(cardDir, "power", "rc6_residency_ms")
	if _, ok := parseResidencyMS(readSysfs(path)); ok {
		return []intelIdleCounter{{id: "power", path: path}}
	}
	return nil
}

// xeIdleCounters finds <device>/tile<T>/gt<M>/gtidle/idle_residency_ms for
// every GT of every tile.
func xeIdleCounters(deviceDir string) []intelIdleCounter {
	var out []intelIdleCounter
	for _, tile := range sortedDirNames(deviceDir) {
		if !isGTDirName(tile, "tile") {
			continue
		}
		tileDir := filepath.Join(deviceDir, tile)
		for _, gt := range sortedDirNames(tileDir) {
			if !isGTDirName(gt, "gt") {
				continue
			}
			path := filepath.Join(tileDir, gt, "gtidle", "idle_residency_ms")
			if _, ok := parseResidencyMS(readSysfs(path)); ok {
				out = append(out, intelIdleCounter{id: tile + "/" + gt, path: path})
			}
		}
	}
	return out
}

// isGTDirName reports whether name is prefix followed by one or more digits
// ("gt0", "tile1"). Everything else under gt/ or the device directory (power,
// hwmon, gt_act_freq_mhz, ...) is not a graphics tile.
func isGTDirName(name, prefix string) bool {
	digits, ok := strings.CutPrefix(name, prefix)
	if !ok || digits == "" {
		return false
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// parseResidencyMS parses one residency attribute: a decimal millisecond count
// and a newline.
func parseResidencyMS(s string) (uint64, bool) {
	v, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// intelBusyPercent turns two readings of one GT's idle counter into a whole
// 0..100 busy percentage: round(100 * (1 - dIdle/dWall)).
//
// ok is false when no figure can be derived from this pair: the counter went
// backwards (a driver reset, a GT power cycle or a wrap), or no wall time
// elapsed. The caller must not publish a number for that pair.
//
// The result is clamped at both ends. The counter has millisecond resolution
// and the two reads are not simultaneous with the hardware's own accounting, so
// dIdle can exceed dWall by a millisecond or two (an idle GPU measured 1007 ms
// of RC6 in a 1000 ms window), which would otherwise be a negative busy figure.
func intelBusyPercent(prev, cur intelIdleSample) (uint32, bool) {
	if cur.ms < prev.ms {
		return 0, false
	}
	wallMS := float64(cur.at.Sub(prev.at)) / float64(time.Millisecond)
	if wallMS <= 0 {
		return 0, false
	}
	busy := math.Round(100 * (1 - float64(cur.ms-prev.ms)/wallMS))
	switch {
	case busy < 0:
		return 0, true
	case busy > 100:
		return 100, true
	}
	return uint32(busy), true
}

// intelHeld is the last good figure published for one card.
type intelHeld struct {
	pct   uint32
	fails int // consecutive ticks without a reading since pct was set
}

// intelUtilSampler derives per-card utilization from the idle counters across
// successive calls. Only the collector's single ticker goroutine may use it, the
// same single-writer rule the CPU baseline relies on, so it holds no lock.
type intelUtilSampler struct {
	prev map[string]map[string]intelIdleSample // statsKey -> GT id -> last read
	held map[string]intelHeld                  // statsKey -> last good figure
}

func newIntelUtilSampler() *intelUtilSampler {
	return &intelUtilSampler{
		prev: map[string]map[string]intelIdleSample{},
		held: map[string]intelHeld{},
	}
}

// sample reads every card's counters at now and returns the utilization of each
// card that has a figure, keyed by statsKey. A card missing from the result has
// no reading: its first sample, a counter that cannot be read, or a run of
// skipped samples longer than intelUtilHoldTicks.
//
// The previous reading is always replaced by the current one, including after a
// reset: a counter that restarted near zero would otherwise stay below the
// remembered value and every following tick would be skipped as a reset too.
func (s *intelUtilSampler) sample(cards []intelCard, now time.Time) map[string]uint32 {
	var out map[string]uint32
	seen := make(map[string]bool, len(cards))
	for _, c := range cards {
		seen[c.statsKey] = true
		pct, ok := s.sampleCard(c, now)
		if !ok {
			pct, ok = s.holdCard(c.statsKey)
		}
		if !ok {
			continue
		}
		if out == nil {
			out = map[string]uint32{}
		}
		out[c.statsKey] = pct
	}
	// A card that left the system (a hot-unplugged Arc, a driver unbound) must
	// not leave stale baselines behind to be compared against if it returns.
	for key := range s.prev {
		if !seen[key] {
			delete(s.prev, key)
			delete(s.held, key)
		}
	}
	return out
}

// sampleCard reads one card and returns the max busy percentage over the GTs
// that produced a valid delta this time.
func (s *intelUtilSampler) sampleCard(c intelCard, now time.Time) (uint32, bool) {
	counters := intelIdleCounters(c)
	prev := s.prev[c.statsKey]
	cur := make(map[string]intelIdleSample, len(counters))
	var best uint32
	have := false
	for _, ctr := range counters {
		ms, ok := parseResidencyMS(readSysfs(ctr.path))
		if !ok {
			continue
		}
		sample := intelIdleSample{ms: ms, at: now}
		cur[ctr.id] = sample
		old, hadPrev := prev[ctr.id]
		if !hadPrev {
			continue // first sample of this GT: nothing to subtract from yet
		}
		pct, ok := intelBusyPercent(old, sample)
		if !ok {
			continue // reset or no elapsed time: skip this GT's sample
		}
		if !have || pct > best {
			best = pct
		}
		have = true
	}
	s.prev[c.statsKey] = cur
	if !have {
		return 0, false
	}
	s.held[c.statsKey] = intelHeld{pct: best}
	return best, true
}

// holdCard publishes a card's last good figure for up to intelUtilHoldTicks
// consecutive ticks without a reading, then forgets it.
func (s *intelUtilSampler) holdCard(key string) (uint32, bool) {
	h, ok := s.held[key]
	if !ok {
		return 0, false
	}
	h.fails++
	if h.fails >= intelUtilHoldTicks {
		delete(s.held, key)
		return 0, false
	}
	s.held[key] = h
	return h.pct, true
}
