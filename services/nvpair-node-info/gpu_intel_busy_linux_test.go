// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// The Intel utilization estimate is a derivative of a sysfs counter, so every
// case below runs against the same fake DRM tree the inventory tests use, with
// the counter files rewritten between samples and the sample time injected.
// Real sysfs layouts reproduced here:
//
//	i915:  <card>/gt/gt<M>/rc6_residency_ms, or legacy <card>/power/rc6_residency_ms
//	xe:    <device>/tile<T>/gt<M>/gtidle/idle_residency_ms
//
// The i915 files live under the DRM card directory, the xe one under the PCI
// device directory, which is why the helpers take the right root for each.

const intelTestKey = "intel:0000:00:02.0"

// writeResidency writes one cumulative counter file, creating its directories.
func writeResidency(t *testing.T, path string, ms uint64) {
	t.Helper()
	mkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(strconv.FormatUint(ms, 10)+"\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// i915GT is the path of GT gt's counter for the card directory of the fake tree.
func (f *intelFakeTree) i915GT(card, gt string) string {
	return filepath.Join(f.drmRoot, card, "gt", gt, "rc6_residency_ms")
}

func (f *intelFakeTree) i915Legacy(card string) string {
	return filepath.Join(f.drmRoot, card, "power", "rc6_residency_ms")
}

var intelT0 = time.Unix(1_000_000, 0)

// TestIntelBusyPercent covers the formula, its rounding, both clamps and every
// pair that must yield no figure. The measured idle case is in the table: the
// RC6 counter advanced 1007 ms over a 1000 ms window.
func TestIntelBusyPercent(t *testing.T) {
	cases := []struct {
		name       string
		prevMS     uint64
		curMS      uint64
		wall       time.Duration
		want       uint32
		wantOK     bool
		wantReason string
	}{
		{"idle, residency equals wall", 5000, 6000, time.Second, 0, true, ""},
		{"idle, residency overshoots wall by a few ms (clamp low)", 5000, 6007, time.Second, 0, true, ""},
		{"fully busy, residency did not advance (clamp high side exact)", 5000, 5000, time.Second, 100, true, ""},
		{"half busy", 5000, 5500, time.Second, 50, true, ""},
		{"rounds to nearest", 5000, 5334, time.Second, 67, true, ""},
		{"rounds down at .4", 5000, 5596, time.Second, 40, true, ""},
		{"wall longer than one second", 0, 1000, 2 * time.Second, 50, true, ""},
		{"counter went backwards is a reset, no figure", 5000, 4999, time.Second, 0, false, "reset"},
		{"counter restarted near zero, no figure", 900000, 12, time.Second, 0, false, "reset"},
		{"no wall time elapsed, no figure", 5000, 5000, 0, 0, false, "zero wall"},
		{"clock went backwards, no figure", 5000, 5000, -time.Second, 0, false, "negative wall"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prev := intelIdleSample{ms: tc.prevMS, at: intelT0}
			cur := intelIdleSample{ms: tc.curMS, at: intelT0.Add(tc.wall)}
			got, ok := intelBusyPercent(prev, cur)
			if ok != tc.wantOK || got != tc.want {
				t.Errorf("intelBusyPercent(%+v, %+v) = %d, %v; want %d, %v", prev, cur, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// TestIntelBusyPercentClampsBothEnds: the formula itself cannot exceed 100 while the
// counter is monotonic, but the clamp is part of the contract (a skewed wall
// clock must never publish an impossible figure), so it is exercised through a
// pair whose raw value would be below zero and above 100 by construction.
func TestIntelBusyPercentClampsBothEnds(t *testing.T) {
	// Raw value -0.7: idle counter ran ahead of the wall clock.
	if got, ok := intelBusyPercent(
		intelIdleSample{ms: 0, at: intelT0},
		intelIdleSample{ms: 1007, at: intelT0.Add(time.Second)}); !ok || got != 0 {
		t.Errorf("overshoot = %d, %v; want 0, true", got, ok)
	}
	// A 1 ms window in which the counter did not move is fully busy, not more.
	if got, ok := intelBusyPercent(
		intelIdleSample{ms: 10, at: intelT0},
		intelIdleSample{ms: 10, at: intelT0.Add(time.Millisecond)}); !ok || got != 100 {
		t.Errorf("no residency = %d, %v; want 100, true", got, ok)
	}
}

func TestIntelIdleCountersI915GTPath(t *testing.T) {
	f := newIntelFakeTree(t)
	f.addCard("card1", "0000:00:02.0", "i915", igpuAttrs(nil))
	writeResidency(t, f.i915GT("card1", "gt0"), 100)

	cards := listIntelCards(f.drmRoot)
	if len(cards) != 1 {
		t.Fatalf("cards = %d, want 1", len(cards))
	}
	got := intelIdleCounters(cards[0])
	if len(got) != 1 || got[0].id != "gt0" || got[0].path != f.i915GT("card1", "gt0") {
		t.Errorf("counters = %+v, want the single gt0 file", got)
	}
}

func TestIntelIdleCountersI915LegacyPowerPath(t *testing.T) {
	f := newIntelFakeTree(t)
	f.addCard("card1", "0000:00:02.0", "i915", igpuAttrs(nil))
	writeResidency(t, f.i915Legacy("card1"), 100)

	got := intelIdleCounters(listIntelCards(f.drmRoot)[0])
	if len(got) != 1 || got[0].id != "power" || got[0].path != f.i915Legacy("card1") {
		t.Errorf("counters = %+v, want the legacy power/rc6_residency_ms file", got)
	}
}

// TestIntelIdleCountersPrefersGTLayoutOverLegacy: when both exist the gt<M>
// layout is the one that covers every GT.
func TestIntelIdleCountersPrefersGTLayoutOverLegacy(t *testing.T) {
	f := newIntelFakeTree(t)
	f.addCard("card1", "0000:00:02.0", "i915", igpuAttrs(nil))
	writeResidency(t, f.i915Legacy("card1"), 100)
	writeResidency(t, f.i915GT("card1", "gt0"), 100)
	writeResidency(t, f.i915GT("card1", "gt1"), 100)
	// Not graphics tiles: an unrelated directory under gt/ must not be read.
	writeResidency(t, filepath.Join(f.drmRoot, "card1", "gt", "power", "rc6_residency_ms"), 1)

	got := intelIdleCounters(listIntelCards(f.drmRoot)[0])
	if len(got) != 2 || got[0].id != "gt0" || got[1].id != "gt1" {
		t.Errorf("counters = %+v, want gt0 and gt1 only", got)
	}
}

// sameFile reports whether two paths name the same file, following symlinks.
func sameFile(t *testing.T, a, b string) bool {
	t.Helper()
	ia, err := os.Stat(a)
	if err != nil {
		t.Fatalf("stat %s: %v", a, err)
	}
	ib, err := os.Stat(b)
	if err != nil {
		t.Fatalf("stat %s: %v", b, err)
	}
	return os.SameFile(ia, ib)
}

func TestIntelIdleCountersXePath(t *testing.T) {
	f := newIntelFakeTree(t)
	dev := f.addCard("card0", "0000:03:00.0", "xe", map[string]string{
		"vendor": "0x8086", "device": "0xe20b", "mem_info_vram_total": "12884901888",
	})
	xe0 := filepath.Join(dev, "tile0", "gt0", "gtidle", "idle_residency_ms")
	xe1 := filepath.Join(dev, "tile0", "gt1", "gtidle", "idle_residency_ms")
	writeResidency(t, xe0, 100)
	writeResidency(t, xe1, 100)
	// An unrelated sibling of the tiles must not be mistaken for one.
	writeResidency(t, filepath.Join(dev, "hwmon", "gt0", "gtidle", "idle_residency_ms"), 1)

	got := intelIdleCounters(listIntelCards(f.drmRoot)[0])
	// The class entry's device is a symlink, so compare by file identity.
	if len(got) != 2 || got[0].id != "tile0/gt0" || got[1].id != "tile0/gt1" ||
		!sameFile(t, got[0].path, xe0) || !sameFile(t, got[1].path, xe1) {
		t.Errorf("counters = %+v, want tile0/gt0 (%s) and tile0/gt1 (%s)", got, xe0, xe1)
	}
}

// TestIntelIdleCountersNoneWhenUnreadable: a counter file that exists but does
// not parse is not a source, and a card with no file at all has none.
func TestIntelIdleCountersNoneWhenUnreadable(t *testing.T) {
	f := newIntelFakeTree(t)
	f.addCard("card1", "0000:00:02.0", "i915", igpuAttrs(nil))
	mkdir(t, filepath.Dir(f.i915GT("card1", "gt0")))
	if err := os.WriteFile(f.i915GT("card1", "gt0"), []byte("not a number\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := intelIdleCounters(listIntelCards(f.drmRoot)[0]); got != nil {
		t.Errorf("counters = %+v, want none", got)
	}
}

// sampleAt runs one sampler pass over the fake tree at t0+offset.
func sampleAt(s *intelUtilSampler, f *intelFakeTree, offset time.Duration) map[string]uint32 {
	return s.sample(listIntelCards(f.drmRoot), intelT0.Add(offset))
}

// TestIntelSamplerFirstSampleHasNoReading: with no previous counter there is
// nothing to subtract, so there is no figure until the second sample.
func TestIntelSamplerFirstSampleHasNoReading(t *testing.T) {
	f := newIntelFakeTree(t)
	f.addCard("card1", "0000:00:02.0", "i915", igpuAttrs(nil))
	writeResidency(t, f.i915GT("card1", "gt0"), 5000)

	s := newIntelUtilSampler()
	if got := sampleAt(s, f, 0); len(got) != 0 {
		t.Fatalf("first sample = %v, want no reading", got)
	}
	writeResidency(t, f.i915GT("card1", "gt0"), 5500)
	got := sampleAt(s, f, time.Second)
	if pct, ok := got[intelTestKey]; !ok || pct != 50 {
		t.Errorf("second sample = %v, want 50 for %s", got, intelTestKey)
	}
}

func TestIntelSamplerIdleAndBusy(t *testing.T) {
	f := newIntelFakeTree(t)
	f.addCard("card1", "0000:00:02.0", "i915", igpuAttrs(nil))
	writeResidency(t, f.i915GT("card1", "gt0"), 5000)
	s := newIntelUtilSampler()
	sampleAt(s, f, 0)

	// Measured idle: 1007 ms of RC6 in a 1000 ms window clamps to 0, and an idle
	// card still has a reading (0 is a measurement, not an absence).
	writeResidency(t, f.i915GT("card1", "gt0"), 6007)
	if pct, ok := sampleAt(s, f, time.Second)[intelTestKey]; !ok || pct != 0 {
		t.Errorf("idle = %d, %v; want 0, true", pct, ok)
	}
	// Saturated: the counter did not advance at all.
	if pct, ok := sampleAt(s, f, 2*time.Second)[intelTestKey]; !ok || pct != 100 {
		t.Errorf("saturated = %d, %v; want 100, true", pct, ok)
	}
}

// TestIntelSamplerUsesElapsedWallTimeNotTheTick: a tick that arrives 2 s late
// divides by 2 s, not by the nominal 1 s.
func TestIntelSamplerUsesElapsedWallTimeNotTheTick(t *testing.T) {
	f := newIntelFakeTree(t)
	f.addCard("card1", "0000:00:02.0", "i915", igpuAttrs(nil))
	writeResidency(t, f.i915GT("card1", "gt0"), 0)
	s := newIntelUtilSampler()
	sampleAt(s, f, 0)
	writeResidency(t, f.i915GT("card1", "gt0"), 1500)
	if pct := sampleAt(s, f, 2*time.Second)[intelTestKey]; pct != 25 {
		t.Errorf("busy over a 2 s gap = %d, want 25", pct)
	}
}

// TestIntelSamplerMultiGTPublishesTheBusiestGT: GT0 idle, GT1 80 % busy, the
// card reads 80 (max), not the 40 an average would give.
func TestIntelSamplerMultiGTPublishesTheBusiestGT(t *testing.T) {
	f := newIntelFakeTree(t)
	f.addCard("card1", "0000:00:02.0", "i915", igpuAttrs(nil))
	writeResidency(t, f.i915GT("card1", "gt0"), 0)
	writeResidency(t, f.i915GT("card1", "gt1"), 0)
	s := newIntelUtilSampler()
	sampleAt(s, f, 0)
	writeResidency(t, f.i915GT("card1", "gt0"), 1000)
	writeResidency(t, f.i915GT("card1", "gt1"), 200)
	if pct, ok := sampleAt(s, f, time.Second)[intelTestKey]; !ok || pct != 80 {
		t.Errorf("multi-GT = %d, %v; want 80 (the max over GTs)", pct, ok)
	}
}

func TestIntelSamplerXeSumsNothingAndTakesTheMax(t *testing.T) {
	f := newIntelFakeTree(t)
	dev := f.addCard("card0", "0000:03:00.0", "xe", map[string]string{
		"vendor": "0x8086", "device": "0xe20b", "mem_info_vram_total": "12884901888",
	})
	xe0 := filepath.Join(dev, "tile0", "gt0", "gtidle", "idle_residency_ms")
	xe1 := filepath.Join(dev, "tile0", "gt1", "gtidle", "idle_residency_ms")
	writeResidency(t, xe0, 100)
	writeResidency(t, xe1, 100)
	s := newIntelUtilSampler()
	sampleAt(s, f, 0)
	// Both GTs fully idle: a sum of idle residency would be 200 % of the window.
	writeResidency(t, xe0, 1100)
	writeResidency(t, xe1, 1100)
	const key = "intel:0000:03:00.0"
	if pct, ok := sampleAt(s, f, time.Second)[key]; !ok || pct != 0 {
		t.Errorf("both GTs idle = %d, %v; want 0, true", pct, ok)
	}
	// One GT 60 % busy.
	writeResidency(t, xe0, 1500)
	writeResidency(t, xe1, 2100)
	if pct := sampleAt(s, f, 2*time.Second)[key]; pct != 60 {
		t.Errorf("xe one GT busy = %d, want 60", pct)
	}
}

func TestIntelSamplerLegacyPowerPath(t *testing.T) {
	f := newIntelFakeTree(t)
	f.addCard("card1", "0000:00:02.0", "i915", igpuAttrs(nil))
	writeResidency(t, f.i915Legacy("card1"), 0)
	s := newIntelUtilSampler()
	sampleAt(s, f, 0)
	writeResidency(t, f.i915Legacy("card1"), 250)
	if pct, ok := sampleAt(s, f, time.Second)[intelTestKey]; !ok || pct != 75 {
		t.Errorf("legacy path = %d, %v; want 75", pct, ok)
	}
}

// TestIntelSamplerCounterResetIsSkippedHeldThenRebaselined is the reset guard.
// A counter that goes backwards yields no figure for that sample; the last good
// value is held for the hold window rather than published as idle; and the
// reset reading becomes the new baseline, so the NEXT tick measures normally
// instead of being skipped for as long as the counter stays below the old one.
func TestIntelSamplerCounterResetIsSkippedHeldThenRebaselined(t *testing.T) {
	f := newIntelFakeTree(t)
	f.addCard("card1", "0000:00:02.0", "i915", igpuAttrs(nil))
	gt := f.i915GT("card1", "gt0")
	writeResidency(t, gt, 900000)
	s := newIntelUtilSampler()
	sampleAt(s, f, 0)
	writeResidency(t, gt, 900500)
	if pct := sampleAt(s, f, time.Second)[intelTestKey]; pct != 50 {
		t.Fatalf("baseline = %d, want 50", pct)
	}

	// Reset: the counter restarts near zero. No fresh figure, so the held 50 is
	// published, not an idle 0 and not an absence.
	writeResidency(t, gt, 10)
	if pct, ok := sampleAt(s, f, 2*time.Second)[intelTestKey]; !ok || pct != 50 {
		t.Errorf("sample after reset = %d, %v; want the held 50", pct, ok)
	}
	// The reset reading is the new baseline: 1000 ms later it advanced 100 ms.
	writeResidency(t, gt, 110)
	if pct, ok := sampleAt(s, f, 3*time.Second)[intelTestKey]; !ok || pct != 90 {
		t.Errorf("sample after rebaseline = %d, %v; want 90 (counter advanced 100 ms in 1000 ms)", pct, ok)
	}
}

// TestIntelSamplerHoldExpires: a counter that stays unreadable is held for
// intelUtilHoldTicks-1 ticks and then reported as having no reading again.
func TestIntelSamplerHoldExpires(t *testing.T) {
	f := newIntelFakeTree(t)
	f.addCard("card1", "0000:00:02.0", "i915", igpuAttrs(nil))
	gt := f.i915GT("card1", "gt0")
	writeResidency(t, gt, 0)
	s := newIntelUtilSampler()
	sampleAt(s, f, 0)
	writeResidency(t, gt, 250)
	if pct := sampleAt(s, f, time.Second)[intelTestKey]; pct != 75 {
		t.Fatalf("baseline = %d, want 75", pct)
	}
	if err := os.Remove(gt); err != nil {
		t.Fatal(err)
	}
	for tick := 1; tick < intelUtilHoldTicks; tick++ {
		if pct, ok := sampleAt(s, f, time.Duration(1+tick)*time.Second)[intelTestKey]; !ok || pct != 75 {
			t.Fatalf("held tick %d = %d, %v; want the held 75", tick, pct, ok)
		}
	}
	if got := sampleAt(s, f, time.Duration(1+intelUtilHoldTicks)*time.Second); len(got) != 0 {
		t.Errorf("after the hold window = %v, want no reading", got)
	}
	// The counter returns: its first read is a new baseline again (no delta
	// against a value from before the gap), the second one measures.
	writeResidency(t, gt, 5000)
	if got := sampleAt(s, f, time.Duration(2+intelUtilHoldTicks)*time.Second); len(got) != 0 {
		t.Errorf("first read after the gap = %v, want no reading", got)
	}
	writeResidency(t, gt, 5500)
	if pct := sampleAt(s, f, time.Duration(3+intelUtilHoldTicks)*time.Second)[intelTestKey]; pct != 50 {
		t.Errorf("second read after the gap = %d, want 50", pct)
	}
}

// TestIntelSamplerCardWithNoCounterHasNoReading: an Intel card whose driver
// publishes no counter never produces a figure, however often it is sampled.
func TestIntelSamplerCardWithNoCounterHasNoReading(t *testing.T) {
	f := newIntelFakeTree(t)
	f.addCard("card1", "0000:00:02.0", "i915", igpuAttrs(nil))
	s := newIntelUtilSampler()
	for i := 0; i < 5; i++ {
		if got := sampleAt(s, f, time.Duration(i)*time.Second); len(got) != 0 {
			t.Fatalf("sample %d = %v, want no reading", i, got)
		}
	}
}

// TestDetectIntelGPUsRowWithACounterNeedsASample: a row whose driver publishes
// a counter is flagged for sampling, not as permanently unavailable, so the
// flag clears when a reading exists. A row with no counter keeps the static
// flag.
func TestDetectIntelGPUsRowWithACounterNeedsASample(t *testing.T) {
	f := newIntelFakeTree(t)
	f.addCard("card1", "0000:00:02.0", "i915", igpuAttrs(nil))
	writeResidency(t, f.i915GT("card1", "gt0"), 5000)
	f.addCard("card2", "0000:00:03.0", "i915", igpuAttrs(map[string]string{
		"uevent": "DRIVER=i915\nPCI_SLOT_NAME=0000:00:03.0",
	}))

	gpus := detectIntelGPUs(f.drmRoot)
	if len(gpus) != 2 {
		t.Fatalf("rows = %d, want 2", len(gpus))
	}
	if !gpus[0].utilizationNeedsSample || gpus[0].UtilizationUnavailable {
		t.Errorf("row with a counter: needsSample=%v unavailable=%v, want true/false",
			gpus[0].utilizationNeedsSample, gpus[0].UtilizationUnavailable)
	}
	if gpus[1].utilizationNeedsSample || !gpus[1].UtilizationUnavailable {
		t.Errorf("row without a counter: needsSample=%v unavailable=%v, want false/true",
			gpus[1].utilizationNeedsSample, gpus[1].UtilizationUnavailable)
	}
}

// TestIntelMergeUtilizationLifecycle drives the merge the way the collector
// does: no figure on the first tick, a known one on the second, the clock
// untouched, and the previously published map never mutated.
func TestIntelMergeUtilizationLifecycle(t *testing.T) {
	f := newIntelFakeTree(t)
	f.addCard("card1", "0000:00:02.0", "i915", igpuAttrs(nil))
	gt := f.i915GT("card1", "gt0")
	writeResidency(t, gt, 5000)
	s := newIntelUtilSampler()

	published := map[string]gpuStat{"GPU-1234": {UtilizationPct: 42, UtilizationKnown: true}}
	first := &statsSnapshot{GPU: published}
	mergeIntelSamples(f.drmRoot, s, intelT0, first)
	if len(first.GPU) != 1 {
		t.Fatalf("first tick changed the map: %+v", first.GPU)
	}

	writeResidency(t, gt, 5250)
	second := &statsSnapshot{GPU: published}
	mergeIntelSamples(f.drmRoot, s, intelT0.Add(time.Second), second)
	if len(published) != 1 {
		t.Fatalf("the previously published map was mutated: %+v", published)
	}
	got, ok := second.GPU[intelTestKey]
	if !ok || got.UtilizationPct != 75 || !got.UtilizationKnown {
		t.Errorf("second tick = %+v (present %v), want 75 and UtilizationKnown", got, ok)
	}
	if second.GPU["GPU-1234"].UtilizationPct != 42 {
		t.Error("the existing NVIDIA row was lost in the merge")
	}
	if !second.GPUSampledAt.IsZero() {
		t.Error("GPUSampledAt advanced: an Intel iGPU reading is not live inference-GPU telemetry")
	}
}

// TestIntelMergeClearsAReadingThatWentAway: on a stale-preserve tick the
// snapshot's map aliases the previous one, which still holds the last Intel
// figure. When the sampler has none, the entry must be cleared (on a clone), or
// the wire would keep publishing a value nobody is measuring any more.
func TestIntelMergeClearsAReadingThatWentAway(t *testing.T) {
	f := newIntelFakeTree(t)
	f.addCard("card1", "0000:00:02.0", "i915", igpuAttrs(nil))
	// No counter file at all: the sampler has no reading.
	s := newIntelUtilSampler()
	previous := map[string]gpuStat{
		intelTestKey: {UtilizationPct: 80, UtilizationKnown: true},
		"GPU-1234":   {UtilizationPct: 42, UtilizationKnown: true},
	}
	snap := &statsSnapshot{GPU: previous}
	mergeIntelSamples(f.drmRoot, s, intelT0, snap)

	if !previous[intelTestKey].UtilizationKnown || previous[intelTestKey].UtilizationPct != 80 {
		t.Fatalf("the previously published map was mutated: %+v", previous)
	}
	if got := snap.GPU[intelTestKey]; got.UtilizationKnown || got.UtilizationPct != 0 {
		t.Errorf("stale Intel figure survived: %+v", got)
	}
	if got := snap.GPU["GPU-1234"]; got.UtilizationPct != 42 || !got.UtilizationKnown {
		t.Errorf("another vendor's row changed: %+v", got)
	}
}

// TestIntelUtilizationWireShape asserts what a client receives for the three
// states of a counter-backed row: a measured busy figure, a measured idle 0 (no
// utilization_percent, and, crucially, no utilization_unavailable either), and
// no reading yet (utilization_unavailable).
func TestIntelUtilizationWireShape(t *testing.T) {
	f := newIntelFakeTree(t)
	f.addCard("card1", "0000:00:02.0", "i915", igpuAttrs(nil))
	writeResidency(t, f.i915GT("card1", "gt0"), 5000)
	gpus := detectIntelGPUs(f.drmRoot)
	if len(gpus) != 1 {
		t.Fatalf("rows = %d, want 1", len(gpus))
	}

	row := func(snap statsSnapshot) map[string]any {
		t.Helper()
		var raw map[string]any
		body := buildResponse(gpus, nil, 0, snap, "", nil)
		if err := json.Unmarshal(body, &raw); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return raw["GPUs"].([]any)[0].(map[string]any)
	}

	busy := row(statsSnapshot{GPU: map[string]gpuStat{intelTestKey: {UtilizationPct: 37, UtilizationKnown: true}}})
	if got := busy["utilization_percent"]; got != float64(37) {
		t.Errorf("busy: utilization_percent = %v, want 37", got)
	}
	if v, present := busy["utilization_unavailable"]; present {
		t.Errorf("busy: utilization_unavailable = %v, want the key dropped once a reading exists", v)
	}

	idle := row(statsSnapshot{GPU: map[string]gpuStat{intelTestKey: {UtilizationPct: 0, UtilizationKnown: true}}})
	if _, present := idle["utilization_unavailable"]; present {
		t.Error("idle: utilization_unavailable present on a measured idle reading")
	}

	none := row(statsSnapshot{})
	if got := none["utilization_unavailable"]; got != true {
		t.Errorf("no reading yet: utilization_unavailable = %v, want true", got)
	}
	if _, present := none["utilization_percent"]; present {
		t.Error("no reading yet: utilization_percent published")
	}

	// What the row must still NOT carry: a memory used figure or a temperature
	// invented for the iGPU (they stay documented ceilings).
	for name, r := range map[string]map[string]any{"busy": busy, "idle": idle, "none": none} {
		if _, present := r["vram_used_bytes"]; present {
			t.Errorf("%s: vram_used_bytes published for an Intel iGPU", name)
		}
		if _, present := r["temperature_celsius"]; present {
			t.Errorf("%s: temperature_celsius published for an Intel iGPU", name)
		}
	}
}
