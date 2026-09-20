// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Linux CPU package power, derived per collector tick from the processor's own
// energy counter — no daemon, no cgo, no privilege escalation.
//
// /sys/class/powercap holds the kernel's powercap view of RAPL (Intel's
// Running Average Power Limit). AMD Zen parts are here too: the amd_rapl
// driver registers its zones under the same intel-rapl class names, so a Zen
// package domain is found by exactly the same walk. Each zone directory
// carries a `name` — "package-0" for the socket, "core" / "uncore" / "dram"
// for the sub-domains — and an `energy_uj` attribute.
//
// energy_uj is a COUNTER, not a rate: total microjoules consumed since boot,
// rolling over at max_energy_range_uj. Watts are therefore a derivative,
// delta-energy over delta-time across two ticks, and the first tick after
// start — or after a rollover, or after a driver reload re-bases the counter —
// produces no figure at all rather than a fabricated one.
//
// The ceiling this cannot cross: since Linux 5.10 energy_uj is mode 0400,
// owned by root. The kernel restricted it because the counter is a side
// channel (power traces recovered AES keys and broke KASLR — CVE-2020-8694),
// and nothing unprivileged replaced it. This service runs as the desktop user,
// so on an ordinary host the read fails with EACCES, the source is reported
// absent in one startup line, and cpu.power_watts is omitted for the life of
// the process.
//
// That is a documented limit, not a bug to route around. Reading it would take
// a setuid helper or a second elevated service, and one wattage figure does not
// justify either — the same reasoning that put the Windows MSR read behind an
// ALREADY elevated helper argues against creating a new privileged surface here.

const powercapClassDir = "/sys/class/powercap"

// raplPackagePrefix is what a socket-scope zone calls itself: "package-0" on a
// single-socket host, "package-1" and up on the others. The sub-domains are
// named "core", "uncore" and "dram" and are deliberately not matched — the
// package is the only figure that answers "what is this CPU drawing".
const raplPackagePrefix = "package-"

// raplPreferredZonePrefix ranks the MSR-backed zones ahead of the rest when a
// host exposes the same package twice. intel-rapl-mmio:0 reports that same
// package-0 through a different aperture and sorts first in plain name order
// ('-' < ':'), so without this the choice between two equivalent readings
// would look arbitrary to anyone reading a field report.
const raplPreferredZonePrefix = "intel-rapl:"

// raplMaxPlausibleWatts bounds a derived figure. A package that appears to
// have drawn more than this between two ticks did not: the counter was
// re-based underneath us (a driver reload, a suspend/resume) and the delta is
// the distance back from the top, not energy. Such a tick re-baselines and
// reports nothing, which is the same answer the very first tick gives.
const raplMaxPlausibleWatts = 1000

// cpuPowerSource resolves once, at collector start, which powercap zone holds
// the package energy counter, so the per-tick cost is one small file read.
//
// It carries the previous sample because watts are a derivative. Only the
// collector's single ticker goroutine calls read, so that state needs no
// synchronization — the same ownership rule statsCollector.prevCPU relies on.
type cpuPowerSource struct {
	// path is the zone's energy_uj, empty when the host exposes no readable
	// package domain (no RAPL at all, or the ordinary 0400 case).
	path string
	// wrapAt is max_energy_range_uj, the value the counter rolls over at.
	wrapAt uint64
	// note explains an empty path in the one startup log line: which zone was
	// found and why it could not be used.
	note string

	prevEnergy uint64
	prevAt     time.Time
	havePrev   bool
}

// findCPUPowerSource locates the package energy counter. The returned source
// has an empty path when the host has no RAPL package zone, or has one this
// process may not read — which is the common case and is not an error.
func findCPUPowerSource() cpuPowerSource {
	return findCPUPowerSourceIn(powercapClassDir)
}

// findCPUPowerSourceIn is findCPUPowerSource against an explicit class root,
// so the search is testable against a fake tree.
func findCPUPowerSourceIn(root string) cpuPowerSource {
	zones := packageZones(root)
	if len(zones) == 0 {
		return cpuPowerSource{note: "no RAPL package domain under " + root}
	}
	var lastNote string
	for _, zone := range zones {
		energy := filepath.Join(zone, "energy_uj")
		if _, err := os.ReadFile(energy); err != nil {
			// Keep the OS's own words: "permission denied" is the answer a
			// reader needs, and it is the answer on every modern kernel.
			lastNote = err.Error()
			continue
		}
		wrap, ok := sysfsUint(filepath.Join(zone, "max_energy_range_uj"))
		if !ok || wrap == 0 {
			lastNote = zone + " reports no max_energy_range_uj, so a counter rollover could not be told from a reset"
			continue
		}
		return cpuPowerSource{path: energy, wrapAt: wrap}
	}
	return cpuPowerSource{note: lastNote}
}

// packageZones lists the powercap zones whose name marks them a package
// domain, MSR-backed zones first and the rest in name order.
func packageZones(root string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	var zones []string
	for _, name := range names {
		zone := filepath.Join(root, name)
		if !strings.HasPrefix(sysfsField(filepath.Join(zone, "name")), raplPackagePrefix) {
			continue
		}
		zones = append(zones, zone)
	}
	sort.SliceStable(zones, func(i, j int) bool {
		return strings.HasPrefix(filepath.Base(zones[i]), raplPreferredZonePrefix) &&
			!strings.HasPrefix(filepath.Base(zones[j]), raplPreferredZonePrefix)
	})
	return zones
}

// read returns the average package power in whole watts over the interval
// since the previous call, and records this sample as the next baseline.
//
// ok is false — and the field is omitted — whenever there is no honest figure
// to give: no source, an unreadable counter this tick, the first sample (no
// interval yet), a non-positive interval, or a delta that can only be a
// counter reset. Each of those re-baselines, so a transient failure costs one
// tick rather than freezing a stale number into the response.
func (s *cpuPowerSource) read(now time.Time) (float64, bool) {
	if s.path == "" {
		return 0, false
	}
	energy, ok := sysfsUint(s.path)
	if !ok {
		s.havePrev = false
		return 0, false
	}
	prevEnergy, prevAt, havePrev := s.prevEnergy, s.prevAt, s.havePrev
	s.prevEnergy, s.prevAt, s.havePrev = energy, now, true
	if !havePrev {
		return 0, false
	}
	elapsed := now.Sub(prevAt).Seconds()
	if elapsed <= 0 {
		return 0, false
	}
	watts := math.Round(float64(raplDelta(prevEnergy, energy, s.wrapAt)) / 1e6 / elapsed)
	if watts > raplMaxPlausibleWatts {
		s.havePrev = false
		return 0, false
	}
	return watts, true
}

// raplDelta is the microjoules consumed between two counter reads, accounting
// for the rollover at wrapAt. A counter that went backwards wrapped: it is
// unsigned and only ever counts up, so the distance is to the top and round.
func raplDelta(prev, cur, wrapAt uint64) uint64 {
	if cur >= prev {
		return cur - prev
	}
	return wrapAt - prev + cur
}

// sysfsUint reads a decimal unsigned sysfs attribute. ok is false when the
// file is missing, unreadable or not a number, so a caller can tell "zero"
// from "unknown".
func sysfsUint(path string) (uint64, bool) {
	v, err := strconv.ParseUint(sysfsField(path), 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
