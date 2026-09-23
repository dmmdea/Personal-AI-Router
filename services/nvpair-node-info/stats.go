// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"math"
	"strconv"
	"strings"
	"time"
)

// gpuStat is the per-adapter dynamic state the service layers on top of the
// static detectGPUs() output. Both fields are zero when the underlying OS
// counter is unavailable; upstream code maps zeros back to "omit from JSON"
// via the omitempty tags on GPUInfo, so clients see a missing field rather
// than a misleading literal zero. That leaves UtilizationPct's absent field
// meaning either "idle" or "could not read", which a client renders the same
// way; a row that has no source at all says so with
// GPUInfo.UtilizationUnavailable instead of relying on the absence.
type gpuStat struct {
	VRAMUsed       uint64
	UtilizationPct uint32
	// TemperatureC is filled only by sources that expose a device thermal
	// readout (the Linux accelerator sampler); zero means unavailable.
	TemperatureC uint32
	// PowerWatts is filled only by sources that meter the device (nvidia-smi
	// power.draw, the amdgpu hwmon PPT input); zero means unavailable, and
	// the omitempty tag on GPUInfo.PowerWatts turns that back into an absent
	// field rather than a device that claims to draw nothing.
	PowerWatts float64
	// UtilizationKnown is true when UtilizationPct is a reading rather than
	// the zero value. Only the Rockchip samplers and the Windows Hailo sampler
	// set it, and it is consulted only for the rows they feed
	// (GPUInfo.utilizationNeedsSample); every other source publishes a stat
	// only when it read something.
	UtilizationKnown bool
	// SharedUsed is the bytes of shared system memory the adapter has
	// allocated (Windows PDH \GPU Adapter Memory\Shared Usage), valid when
	// SharedUsedKnown. Only rows with GPUInfo.usedIncludesShared read it: an
	// integrated GPU, whose pool is its dedicated aperture plus shared memory.
	SharedUsed      uint64
	SharedUsedKnown bool
}

// statsSnapshot is the dynamic bundle the HTTP handler reads without locking.
// Darwin combines independently published system and GPU samples so a slow
// ioreg call cannot delay CPU or memory telemetry.
// GPUInventory is normally nil. It carries an adapter list a collector
// re-detected after startup, which main.go's mergeGPUInventory folds into the
// list detected once at boot: the Darwin collector fills it from its retrying
// ioreg sample, and the Windows collector from its re-detect loop, so an
// enumeration that failed or came up empty at boot recovers without restarting
// the service.
//
// Unsupported collectors publish a zero-valued snapshot — GPU is a nil map
// (omitempty semantics for downstream lookups come from the per-GPU omitempty
// tags, not from an empty-map check here), CPUUtilPct is 0, MemUsedBytes is 0.
// buildResponse treats each subsystem's zero-value as "unknown" and drops the
// corresponding field per the normal omitempty rules.
type statsSnapshot struct {
	GPU          map[string]gpuStat
	GPUInventory []GPUInfo
	// GPUHardwareKeys is every statsKey -> GPUInfo.hardwareKey pairing a
	// re-detection has reported while the process ran, including statsKeys no
	// longer detected. Only the Windows collector fills it (its statsKey, an
	// adapter LUID, can be reissued); mergeGPUInventory uses it to give a
	// startup row whose PCI address was unreadable at boot the identity a
	// later reissue of that card is matched by. Never mutated after publish.
	GPUHardwareKeys map[string]string
	// GPUSampledAt is the collection time of the latest usable GPU
	// utilization sample. A zero value means no usable sample has ever been
	// collected. Failed collection attempts retain the previous timestamp so
	// consumers can distinguish stale telemetry from freshly sampled data.
	GPUSampledAt time.Time
	CPUUtilPct   uint32
	// CPUTempC is the CPU package temperature in whole degrees Celsius, zero
	// when the host has no driverless source for it (see cputemp_linux.go).
	CPUTempC uint32
	// CPUPowerWatts is the CPU package power draw in whole watts, zero when
	// the host exposes no readable energy counter — which is most Linux
	// hosts, where the powercap counter is root-only (see cpupower_linux.go).
	CPUPowerWatts float64
	MemUsedBytes  uint64
}

// applyGPUStats publishes a usable GPU sample or preserves the last usable
// sample after a failed collection. Before the first usable sample, callers may
// still publish partial dynamic fields (for example VRAM usage) with a zero
// sampledAt; those fields remain display-only until utilization becomes valid.
func applyGPUStats(previous statsSnapshot, next *statsSnapshot, sampled map[string]gpuStat, sampledAt time.Time) {
	if sampledAt.IsZero() && !previous.GPUSampledAt.IsZero() {
		next.GPU = previous.GPU
		next.GPUSampledAt = previous.GPUSampledAt
		return
	}
	next.GPU = sampled
	next.GPUSampledAt = sampledAt
}

// parseWatts decodes one power reading into whole watts.
//
// nvidia-smi prints power.draw with two decimals under --format=csv,nounits
// ("6.89") and "[N/A]" for a card that does not meter itself, a driver that
// will not say, and several virtualized SKUs. Anything that is not a finite,
// non-negative number is rejected rather than published as 0, because a zero
// renders as "this device is drawing no power" — a claim, and a wrong one on
// a card whose meter simply is not there.
//
// Whole watts is the published resolution: the node inventory is a display
// surface, the reading moves by more than a watt between two ticks anyway,
// and a fractional value would only produce noisier change events downstream.
//
// Lives in the platform-neutral file so the Linux collector and the Windows
// nvidia-smi poller decode the same text the same way.
func parseWatts(s string) (float64, bool) {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0, false
	}
	return math.Round(v), true
}

// parseEngineInstance pulls the adapter-LUID key and the engine-type tag
// out of a PDH "GPU Engine" counter instance name, which looks like:
//
//	pid_1234_luid_0x00000000_0x000054f0_phys_0_eng_0_engtype_3D
//
// We need the LUID portion to join against luidKey() (which is how the
// DXGI-enumerated adapters identify themselves) and the engine-type to
// bucket utilization before the across-engine max. The returned luidKey
// is lowercase to match the normalization the VRAM counter reader does
// on its side, so both counter streams key into the same adapter map.
//
// Lives in a platform-neutral file (no build tag) so it's unit-testable
// on any host — the string format is stable across Windows versions and
// doesn't touch the PDH API itself.
func parseEngineInstance(s string) (luidKey, engtype string, ok bool) {
	s = strings.ToLower(s)
	luidIdx := strings.Index(s, "luid_")
	if luidIdx < 0 {
		return "", "", false
	}
	rest := s[luidIdx:]
	engIdx := strings.Index(rest, "_eng_")
	if engIdx < 0 {
		return "", "", false
	}
	luidKey = rest[:engIdx]
	const typeMarker = "_engtype_"
	typeIdx := strings.Index(rest, typeMarker)
	if typeIdx < 0 {
		return "", "", false
	}
	engtype = rest[typeIdx+len(typeMarker):]
	if engtype == "" {
		return "", "", false
	}
	return luidKey, engtype, true
}

// aggregateUtilization implements Task Manager's "overall GPU %" calc:
//
//  1. Sum per-process percentages within each (luid, engine-type) bucket.
//     Two apps each running the 3D engine at 30 % really is 60 % engine
//     load; the PDH counter is per-process-time, so summing is correct.
//  2. Clamp each bucket to 100. Sampling jitter can occasionally push a
//     bucket slightly over, and we don't want to surface >100 % to users.
//  3. Take the max across engine types for each luid. The number users
//     recognize as "the GPU is busy" is the busiest engine on that adapter,
//     not the average — a compute-bound ML job with an idle 3D engine is
//     still a fully-busy GPU.
//
// Result is rounded to whole percent. Sub-percent precision is below the
// visual resolution of any reasonable UI and would only generate noisy
// node/updated events from the change-detection plumbing downstream.
func aggregateUtilization(items map[string]float64) map[string]uint32 {
	byEngine := map[string]map[string]float64{}
	for instance, v := range items {
		luid, engtype, ok := parseEngineInstance(instance)
		if !ok || v < 0 {
			continue
		}
		m := byEngine[luid]
		if m == nil {
			m = map[string]float64{}
			byEngine[luid] = m
		}
		m[engtype] += v
	}
	out := make(map[string]uint32, len(byEngine))
	for luid, m := range byEngine {
		var maxPct float64
		for _, p := range m {
			if p > 100 {
				p = 100
			}
			if p > maxPct {
				maxPct = p
			}
		}
		out[luid] = uint32(math.Round(maxPct))
	}
	return out
}

// foldSharedUsage adds PDH \GPU Adapter Memory(*)\Shared Usage readings to
// the per-adapter map, keyed the way the Dedicated Usage fold keys them (the
// lowercased "luid_..." instance name). A negative reading or an instance that
// is not an adapter LUID is skipped rather than recorded as 0 bytes used.
// Platform-neutral so it is testable anywhere; only the Windows collector
// calls it.
func foldSharedUsage(out map[string]gpuStat, readings map[string]int64) {
	for name, v := range readings {
		if v < 0 {
			continue
		}
		key := strings.ToLower(name)
		if !strings.HasPrefix(key, "luid_") {
			continue
		}
		s := out[key]
		s.SharedUsed = uint64(v)
		s.SharedUsedKnown = true
		out[key] = s
	}
}
