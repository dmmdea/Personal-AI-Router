// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// writeZone builds one powercap zone directory. mode is applied to energy_uj
// so the unreadable case — which is the normal case on a modern kernel — can
// be reproduced without root.
func writeZone(t *testing.T, root, dir, name, energy, wrap string, mode os.FileMode) string {
	t.Helper()
	zone := filepath.Join(root, dir)
	if err := os.MkdirAll(zone, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(zone, "name"), []byte(name+"\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	if energy != "" {
		if err := os.WriteFile(filepath.Join(zone, "energy_uj"), []byte(energy+"\n"), mode); err != nil {
			t.Fatal(err)
		}
	}
	if wrap != "" {
		if err := os.WriteFile(filepath.Join(zone, "max_energy_range_uj"), []byte(wrap+"\n"), 0o444); err != nil {
			t.Fatal(err)
		}
	}
	return zone
}

// TestFindCPUPowerSourceInPicksThePackageZone pins the search: the package
// domain, not one of its sub-domains, and the MSR-backed view when a host
// publishes the same package twice.
func TestFindCPUPowerSourceInPicksThePackageZone(t *testing.T) {
	root := t.TempDir()
	// Sub-domains first in name order, so a search that took the first zone
	// it could read would take the wrong one.
	writeZone(t, root, "intel-rapl:0:0", "core", "111", "262143328850", 0o644)
	writeZone(t, root, "intel-rapl:0:1", "uncore", "222", "262143328850", 0o644)
	pkg := writeZone(t, root, "intel-rapl:0", "package-0", "333", "262143328850", 0o644)

	got := findCPUPowerSourceIn(root)
	if got.path != filepath.Join(pkg, "energy_uj") {
		t.Fatalf("path = %q, want the package zone's counter", got.path)
	}
	if got.wrapAt != 262143328850 {
		t.Fatalf("wrapAt = %d", got.wrapAt)
	}
}

// TestFindCPUPowerSourceInPrefersTheMSRZone: intel-rapl-mmio:0 reports the
// same package through a different aperture and sorts first by plain name
// order ('-' < ':'), so without the explicit ranking the pick would be
// arbitrary.
func TestFindCPUPowerSourceInPrefersTheMSRZone(t *testing.T) {
	root := t.TempDir()
	writeZone(t, root, "intel-rapl-mmio:0", "package-0", "999", "262143328850", 0o644)
	msr := writeZone(t, root, "intel-rapl:0", "package-0", "333", "262143328850", 0o644)

	if got := findCPUPowerSourceIn(root); got.path != filepath.Join(msr, "energy_uj") {
		t.Fatalf("path = %q, want the MSR-backed zone", got.path)
	}
}

// TestFindCPUPowerSourceInUnreadableCounter is the measured shape of the
// real hosts: the zone exists, energy_uj is 0400 root, and an unprivileged
// reader gets EACCES. That is an absent source carrying the OS's own reason,
// never an error and never a fabricated zero.
func TestFindCPUPowerSourceInUnreadableCounter(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: the 0400 counter would be readable, which is the case this test is not about")
	}
	root := t.TempDir()
	writeZone(t, root, "intel-rapl:0", "package-0", "333", "262143328850", 0o000)

	got := findCPUPowerSourceIn(root)
	if got.path != "" {
		t.Fatalf("path = %q, want an absent source for an unreadable counter", got.path)
	}
	if got.note == "" {
		t.Fatal("an absent source must carry the reason it is absent")
	}
	t.Logf("reason: %s", got.note)

	// And it stays silent per tick rather than reporting zero watts.
	if w, ok := got.read(time.Now()); ok || w != 0 {
		t.Fatalf("read = %v, %v; want 0, false", w, ok)
	}
}

// TestFindCPUPowerSourceInNoRAPL covers the host that has no powercap class
// at all (a VM, an Arm board): absent, with a reason, and no panic.
func TestFindCPUPowerSourceInNoRAPL(t *testing.T) {
	got := findCPUPowerSourceIn(filepath.Join(t.TempDir(), "absent"))
	if got.path != "" || got.note == "" {
		t.Fatalf("got %+v, want an absent source with a reason", got)
	}

	// A zone with no max_energy_range_uj is equally unusable: without the
	// rollover point a counter reset cannot be told from a rollover.
	root := t.TempDir()
	writeZone(t, root, "intel-rapl:0", "package-0", "333", "", 0o644)
	if got := findCPUPowerSourceIn(root); got.path != "" {
		t.Fatalf("path = %q, want an absent source when the wrap point is unknown", got.path)
	}
}

// TestCPUPowerSourceReadDerivesWatts walks the counter the way the collector
// does: the first tick has no interval and reports nothing; the second
// divides the delta by the elapsed time.
func TestCPUPowerSourceReadDerivesWatts(t *testing.T) {
	root := t.TempDir()
	zone := writeZone(t, root, "intel-rapl:0", "package-0", "1000000", "262143328850", 0o644)
	energy := filepath.Join(zone, "energy_uj")
	s := findCPUPowerSourceIn(root)
	if s.path != energy {
		t.Fatalf("path = %q", s.path)
	}

	base := time.Date(2026, 9, 20, 16, 0, 0, 0, time.UTC)
	if w, ok := s.read(base); ok {
		t.Fatalf("first sample produced %v watts; a counter needs two reads", w)
	}

	// 28 joules over 2 seconds is 14 W.
	if err := os.WriteFile(energy, []byte("29000000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w, ok := s.read(base.Add(2 * time.Second))
	if !ok || w != 14 {
		t.Fatalf("read = %v, %v; want 14, true", w, ok)
	}

	// 45.4 joules over 1 second rounds to 45 W — whole watts is the
	// published resolution.
	if err := os.WriteFile(energy, []byte("74400000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if w, ok := s.read(base.Add(3 * time.Second)); !ok || w != 45 {
		t.Fatalf("read = %v, %v; want 45, true", w, ok)
	}
}

// TestCPUPowerSourceReadHandlesWrap is the whole reason max_energy_range_uj
// is resolved at open: the counter rolls over roughly every hour on a busy
// package, and a naive subtraction would report a negative — or, unsigned, an
// astronomical — delta at that moment.
func TestCPUPowerSourceReadHandlesWrap(t *testing.T) {
	const wrap = 262143328850
	root := t.TempDir()
	zone := writeZone(t, root, "intel-rapl:0", "package-0", "262143328000", strconv.FormatUint(wrap, 10), 0o644)
	energy := filepath.Join(zone, "energy_uj")
	s := findCPUPowerSourceIn(root)

	base := time.Date(2026, 9, 20, 16, 0, 0, 0, time.UTC)
	s.read(base) // prime at 850 uJ below the top

	// Rolls over and lands 19_999_150 uJ past zero: 850 + 19_999_150 = 20 J
	// in 1 s = 20 W.
	if err := os.WriteFile(energy, []byte("19999150\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if w, ok := s.read(base.Add(time.Second)); !ok || w != 20 {
		t.Fatalf("across a rollover read = %v, %v; want 20, true", w, ok)
	}

	// The arithmetic itself, stated plainly.
	if got := raplDelta(wrap-850, 19_999_150, wrap); got != 20_000_000 {
		t.Fatalf("raplDelta across the wrap = %d, want 20000000", got)
	}
	if got := raplDelta(100, 500, wrap); got != 400 {
		t.Fatalf("raplDelta = %d, want 400", got)
	}
}

// TestCPUPowerSourceReadRejectsAReset: a counter re-based under us (a driver
// reload, a resume) looks like a rollover but is not. The figure it would
// produce is absurd, so the tick publishes nothing and re-baselines instead of
// putting a five-digit wattage on a node card.
func TestCPUPowerSourceReadRejectsAReset(t *testing.T) {
	const wrap = 262143328850
	root := t.TempDir()
	zone := writeZone(t, root, "intel-rapl:0", "package-0", "200000000000", strconv.FormatUint(wrap, 10), 0o644)
	energy := filepath.Join(zone, "energy_uj")
	s := findCPUPowerSourceIn(root)

	base := time.Date(2026, 9, 20, 16, 0, 0, 0, time.UTC)
	s.read(base)
	if err := os.WriteFile(energy, []byte("5000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if w, ok := s.read(base.Add(time.Second)); ok {
		t.Fatalf("a counter reset produced %v watts; want nothing", w)
	}
	// Re-baselined: the next interval is measured from the reset value, so
	// one bad tick costs one reading rather than poisoning the next.
	if err := os.WriteFile(energy, []byte("15000000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if w, ok := s.read(base.Add(2 * time.Second)); ok {
		t.Fatalf("read = %v, %v; want nothing, the baseline was dropped", w, ok)
	}
	if err := os.WriteFile(energy, []byte("30000000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if w, ok := s.read(base.Add(3 * time.Second)); !ok || w != 15 {
		t.Fatalf("read = %v, %v; want 15, true", w, ok)
	}
}

// TestCPUPowerSourceReadRejectsANonPositiveInterval: two reads inside the
// same instant divide by zero. A clock that went backwards does worse.
func TestCPUPowerSourceReadRejectsANonPositiveInterval(t *testing.T) {
	root := t.TempDir()
	writeZone(t, root, "intel-rapl:0", "package-0", "1000000", "262143328850", 0o644)
	s := findCPUPowerSourceIn(root)
	now := time.Date(2026, 9, 20, 16, 0, 0, 0, time.UTC)
	s.read(now)
	if _, ok := s.read(now); ok {
		t.Fatal("a zero interval must produce no figure")
	}
	if _, ok := s.read(now.Add(-time.Second)); ok {
		t.Fatal("a backwards clock must produce no figure")
	}
}

// TestLiveCPUPackagePower runs against a real host when NVPAIR_LIVE_POWER=1.
//
// The unit tests above cover every arithmetic path against a fake tree. What
// they cannot answer is the question this change actually has to settle on
// Linux: whether the process that runs this service may read the counter at
// all. So this prints what it found — the zone, the mode, and the derived
// wattage or the reason there is none — and fails only if a counter it CAN
// read yields no figure across two ticks, which would mean the derivative is
// broken rather than the permission.
func TestLiveCPUPackagePower(t *testing.T) {
	if os.Getenv("NVPAIR_LIVE_POWER") != "1" {
		t.Skip("set NVPAIR_LIVE_POWER=1 to run against this host's RAPL counters")
	}
	for _, zone := range packageZones(powercapClassDir) {
		energy := filepath.Join(zone, "energy_uj")
		info, err := os.Stat(energy)
		mode := "absent"
		if err == nil {
			mode = info.Mode().String()
		}
		_, readErr := os.ReadFile(energy)
		t.Logf("zone %s: name=%q mode=%s read=%v", zone, sysfsField(filepath.Join(zone, "name")), mode, readErr)
	}

	s := findCPUPowerSource()
	if s.path == "" {
		t.Logf("no readable CPU power source on this host: %s", s.note)
		t.Log("cpu.power_watts is omitted here; this is the documented ceiling, not a failure")
		return
	}
	t.Logf("CPU power source: %s (wraps at %d uJ)", s.path, s.wrapAt)
	if _, ok := s.read(time.Now()); ok {
		t.Error("the first sample produced a figure; a counter needs two reads")
	}
	time.Sleep(time.Second)
	w, ok := s.read(time.Now())
	if !ok {
		t.Fatal("a readable counter produced no figure across two ticks")
	}
	t.Logf("CPU package power: %.0f W", w)
	if w <= 0 {
		t.Errorf("derived %v W from a live counter", w)
	}
}
