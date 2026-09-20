// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"nvpair-shared/hostsensors"
)

// fakeSensor scripts read results; each read consumes the next entry and the
// last one repeats.
type fakeSensor struct {
	script []readResult
	reads  int
	closed bool
	// watts is what power() reports, and powerOK whether it reports
	// anything at all. The default zero value is a part with no energy
	// counter, which is what most of these cases are about.
	watts   float64
	powerOK bool
	// powerCalls counts power() so a test can assert it was sampled on the
	// same tick as the temperature rather than on a timer of its own.
	powerCalls int
}

type readResult struct {
	c   uint32
	err error
}

func (f *fakeSensor) read() (uint32, error) {
	i := f.reads
	if i >= len(f.script) {
		i = len(f.script) - 1
	}
	f.reads++
	return f.script[i].c, f.script[i].err
}

func (f *fakeSensor) power(time.Time) (float64, bool) {
	f.powerCalls++
	return f.watts, f.powerOK
}

func (f *fakeSensor) tjMax() uint32 { return 100 }

func (f *fakeSensor) close() { f.closed = true }

// opener counts opens and hands out sensors from a queue; an empty queue
// means open fails with failErr.
type opener struct {
	queue   []*fakeSensor
	opens   int
	failErr error
}

func (o *opener) open() (packageSensor, error) {
	o.opens++
	if len(o.queue) == 0 {
		return nil, o.failErr
	}
	s := o.queue[0]
	o.queue = o.queue[1:]
	return s, nil
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

const (
	tickInterval = 2 * time.Second
	tickRetry    = 30 * time.Second
)

// TestSamplerTickPublishesReadingsAndSurvivesOneBadRead: a good read
// publishes a stamped reading; a single failed read publishes the reason at
// the normal cadence without a reopen; the next good read resumes.
func TestSamplerTickPublishesReadingsAndSurvivesOneBadRead(t *testing.T) {
	now := time.Date(2026, 9, 19, 22, 30, 0, 0, time.UTC)
	fs := &fakeSensor{script: []readResult{{c: 58}, {err: errors.New("reading not valid")}, {c: 60}}}
	o := &opener{queue: []*fakeSensor{fs}}
	s := newSampler(tickInterval, tickRetry, quietLog(), o.open, func() time.Time { return now })
	st := &samplerState{}

	if r := s.report(); r.CPU != nil || r.Error != "starting" {
		t.Fatalf("before the first tick: %+v", r)
	}
	if d := s.tick(st); d != tickInterval {
		t.Fatalf("delay after a good read = %s, want %s", d, tickInterval)
	}
	r := s.report()
	if r.CPU == nil || r.CPU.PackageCelsius != 58 || r.CPU.TjMaxCelsius != 100 || r.CPU.Source != sourceIntelMSR || !r.CPU.SampledAt.Equal(now) || r.Error != "" {
		t.Fatalf("first reading: %+v cpu=%+v", r, r.CPU)
	}

	if d := s.tick(st); d != tickInterval {
		t.Fatalf("delay after one bad read = %s, want %s (no reopen)", d, tickInterval)
	}
	if r := s.report(); r.CPU != nil || r.Error != "reading not valid" {
		t.Fatalf("after one bad read: %+v", r)
	}
	if o.opens != 1 || fs.closed {
		t.Fatalf("one bad read must not reopen: opens=%d closed=%v", o.opens, fs.closed)
	}

	s.tick(st)
	if r := s.report(); r.CPU == nil || r.CPU.PackageCelsius != 60 || r.Error != "" {
		t.Fatalf("recovered reading: %+v", r)
	}
	if st.failures != 0 {
		t.Fatalf("failure streak not reset: %d", st.failures)
	}
}

// TestSamplerTickReopensAfterRepeatedReadFailures: after
// sensorReopenAfterFailures consecutive failures the executor is closed, the
// retry delay applies, and the next tick reopens and reads from the new
// sensor.
func TestSamplerTickReopensAfterRepeatedReadFailures(t *testing.T) {
	dead := &fakeSensor{script: []readResult{{c: 55}, {err: errors.New("HRESULT 0x80070006")}}}
	fresh := &fakeSensor{script: []readResult{{c: 57}}}
	o := &opener{queue: []*fakeSensor{dead, fresh}}
	s := newSampler(tickInterval, tickRetry, quietLog(), o.open, time.Now)
	st := &samplerState{}

	s.tick(st)
	if r := s.report(); r.CPU == nil || r.CPU.PackageCelsius != 55 {
		t.Fatalf("first reading: %+v", r)
	}
	for i := 1; i < sensorReopenAfterFailures; i++ {
		if d := s.tick(st); d != tickInterval {
			t.Fatalf("failure %d: delay = %s, want %s", i, d, tickInterval)
		}
		if dead.closed {
			t.Fatalf("failure %d: closed too early", i)
		}
	}
	if d := s.tick(st); d != tickRetry {
		t.Fatalf("failure %d: delay = %s, want retry %s", sensorReopenAfterFailures, d, tickRetry)
	}
	if !dead.closed || st.sensor != nil {
		t.Fatalf("the failed executor must be closed and dropped: closed=%v sensor=%v", dead.closed, st.sensor)
	}
	if r := s.report(); r.CPU != nil || r.Error != "HRESULT 0x80070006" {
		t.Fatalf("report while reopening: %+v", r)
	}

	if d := s.tick(st); d != tickInterval {
		t.Fatalf("delay after reopen = %s, want %s", d, tickInterval)
	}
	if o.opens != 2 {
		t.Fatalf("opens = %d, want 2", o.opens)
	}
	if r := s.report(); r.CPU == nil || r.CPU.PackageCelsius != 57 || r.Error != "" {
		t.Fatalf("reading from the reopened sensor: %+v", r)
	}
}

// TestSamplerTickRetriesOpenUntilItSucceeds: with no sensor the report
// carries the reason and no CPU, at the retry cadence; once open succeeds a
// reading appears.
func TestSamplerTickRetriesOpenUntilItSucceeds(t *testing.T) {
	o := &opener{failErr: errors.New("PawnIO is not installed")}
	s := newSampler(tickInterval, tickRetry, quietLog(), o.open, time.Now)
	st := &samplerState{}

	for i := 0; i < 3; i++ {
		if d := s.tick(st); d != tickRetry {
			t.Fatalf("attempt %d: delay = %s, want retry %s", i, d, tickRetry)
		}
		if r := s.report(); r.CPU != nil || r.Error != "PawnIO is not installed" {
			t.Fatalf("attempt %d: %+v", i, r)
		}
	}
	if o.opens != 3 {
		t.Fatalf("opens = %d, want 3", o.opens)
	}
	o.queue = []*fakeSensor{{script: []readResult{{c: 61}}}}
	if d := s.tick(st); d != tickInterval {
		t.Fatalf("delay after late open = %s, want %s", d, tickInterval)
	}
	if r := s.report(); r.CPU == nil || r.CPU.PackageCelsius != 61 || r.Error != "" {
		t.Fatalf("reading after late open: %+v", r)
	}
}

// TestSamplerStopClosesSensor guards shutdown through the real loop: Stop
// returns promptly during a long wait and releases the executor.
func TestSamplerStopClosesSensor(t *testing.T) {
	fs := &fakeSensor{script: []readResult{{c: 50}}}
	o := &opener{queue: []*fakeSensor{fs}}
	s := newSampler(time.Hour, time.Hour, quietLog(), o.open, time.Now)
	go s.run()
	deadline := time.Now().Add(5 * time.Second)
	for s.report().CPU == nil {
		if time.Now().After(deadline) {
			t.Fatal("no reading from the running loop")
		}
		time.Sleep(time.Millisecond)
	}
	done := make(chan struct{})
	go func() { s.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return")
	}
	if !fs.closed {
		t.Fatal("Stop must close the sensor")
	}
}

// fakeBoard scripts board read results the same way fakeSensor scripts CPU
// ones; the last entry repeats.
type fakeBoard struct {
	script []boardResult
	reads  int
	note   string
	closed bool
}

type boardResult struct {
	reading hostsensors.BoardReading
	err     error
}

func (f *fakeBoard) read() (hostsensors.BoardReading, error) {
	i := f.reads
	if i >= len(f.script) {
		i = len(f.script) - 1
	}
	f.reads++
	return f.script[i].reading, f.script[i].err
}

func (f *fakeBoard) openNote() string { return f.note }

func (f *fakeBoard) close() { f.closed = true }

// boardOpener hands out board sensors from a queue; an empty queue fails.
type boardOpener struct {
	queue   []*fakeBoard
	opens   int
	failErr error
}

func (o *boardOpener) open() (boardSensor, error) {
	o.opens++
	if len(o.queue) == 0 {
		return nil, o.failErr
	}
	b := o.queue[0]
	o.queue = o.queue[1:]
	return b, nil
}

func nuvotonReading() hostsensors.BoardReading {
	return hostsensors.BoardReading{
		Chip:    "Nuvoton NCT6798D",
		ChipID:  "0xD4/0x2B",
		Vendor:  "ASUSTeK COMPUTER INC.",
		Product: "ROG STRIX X299-E GAMING II",
		Temperatures: []hostsensors.BoardTemp{
			{Label: "CPU socket", Input: "CPUTIN", Role: hostsensors.RoleCPUSocket, Celsius: 40},
			{Label: "Motherboard", Input: "SYSTIN", Role: hostsensors.RoleMotherboard, Celsius: 30.5},
		},
		Fans:       []hostsensors.BoardFan{{Index: 0, Label: "Fan 1", RPM: 1100}},
		VcoreVolts: 1.12,
		Source:     sourceSuperIO,
	}
}

// TestSamplerTickPublishesTheBoardBesideTheCPU: both sections land in one
// report and the board reading is stamped by the sampler, not by the sensor.
func TestSamplerTickPublishesTheBoardBesideTheCPU(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	o := &opener{queue: []*fakeSensor{{script: []readResult{{c: 58}}}}}
	bo := &boardOpener{queue: []*fakeBoard{{script: []boardResult{{reading: nuvotonReading()}}}}}
	s := newSampler(tickInterval, tickRetry, quietLog(), o.open, func() time.Time { return now })
	s.openBoard = bo.open
	st := &samplerState{}

	if d := s.tick(st); d != tickInterval {
		t.Fatalf("delay = %s, want %s", d, tickInterval)
	}
	r := s.report()
	if r.CPU == nil || r.CPU.PackageCelsius != 58 {
		t.Fatalf("cpu = %+v", r.CPU)
	}
	if r.Board == nil {
		t.Fatal("the report carries no board section")
	}
	if r.Board.Chip != "Nuvoton NCT6798D" || r.Board.Product != "ROG STRIX X299-E GAMING II" {
		t.Fatalf("board = %+v", r.Board)
	}
	if !r.Board.SampledAt.Equal(now.UTC()) {
		t.Fatalf("board sampled_at = %s, want %s", r.Board.SampledAt, now.UTC())
	}
	if r.Error != "" {
		t.Fatalf("error = %q, want empty", r.Error)
	}
}

// TestSamplerTickKeepsEachSensorsFailureToItself: the two sensors fail apart.
// A host with no supported Super I/O chip must still publish its CPU
// temperature, and a host whose CPU sensor is gone must still publish its
// board sensors — the regression this separation exists to prevent.
func TestSamplerTickKeepsEachSensorsFailureToItself(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)

	t.Run("no board chip", func(t *testing.T) {
		o := &opener{queue: []*fakeSensor{{script: []readResult{{c: 58}}}}}
		bo := &boardOpener{failErr: errors.New("no supported Super I/O chip")}
		s := newSampler(tickInterval, tickRetry, quietLog(), o.open, func() time.Time { return now })
		s.openBoard = bo.open
		s.tick(&samplerState{})

		r := s.report()
		if r.CPU == nil || r.CPU.PackageCelsius != 58 {
			t.Fatalf("an unsupported board cost the CPU reading: %+v", r)
		}
		if r.Board != nil {
			t.Fatalf("board = %+v, want nil", r.Board)
		}
		if r.Error != "" {
			t.Fatalf("error = %q: an absent board section is not a CPU fault", r.Error)
		}
	})

	t.Run("no CPU sensor", func(t *testing.T) {
		o := &opener{failErr: errors.New("PawnIO is not installed")}
		bo := &boardOpener{queue: []*fakeBoard{{script: []boardResult{{reading: nuvotonReading()}}}}}
		s := newSampler(tickInterval, tickRetry, quietLog(), o.open, func() time.Time { return now })
		s.openBoard = bo.open
		s.tick(&samplerState{})

		r := s.report()
		if r.CPU != nil {
			t.Fatalf("cpu = %+v, want nil", r.CPU)
		}
		if r.Error != "PawnIO is not installed" {
			t.Fatalf("error = %q", r.Error)
		}
		if r.Board == nil {
			t.Fatal("a missing CPU sensor took the board section down with it")
		}
	})
}

// TestSamplerTickReopensTheBoardAfterRepeatedFailures and holds off on the
// retry cadence rather than reopening on every tick.
func TestSamplerTickReopensTheBoardAfterRepeatedFailures(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	clock := now
	failing := &fakeBoard{script: []boardResult{{err: errors.New("the module stopped answering")}}}
	healthy := &fakeBoard{script: []boardResult{{reading: nuvotonReading()}}}
	o := &opener{queue: []*fakeSensor{{script: []readResult{{c: 58}}}}}
	bo := &boardOpener{queue: []*fakeBoard{failing, healthy}}
	s := newSampler(tickInterval, tickRetry, quietLog(), o.open, func() time.Time { return clock })
	s.openBoard = bo.open
	st := &samplerState{}

	for i := 0; i < sensorReopenAfterFailures; i++ {
		s.tick(st)
		if r := s.report(); r.Board != nil {
			t.Fatalf("tick %d published a board section from a failing sensor", i)
		}
	}
	if !failing.closed {
		t.Fatal("the failing board sensor was not closed after its failure streak")
	}
	if bo.opens != 1 {
		t.Fatalf("opens = %d, want 1 so far", bo.opens)
	}

	// Still inside the retry window: no second open attempt.
	clock = now.Add(tickRetry / 2)
	s.tick(st)
	if bo.opens != 1 {
		t.Fatalf("opens = %d during the retry hold-off, want 1", bo.opens)
	}

	clock = now.Add(tickRetry + time.Second)
	s.tick(st)
	if bo.opens != 2 {
		t.Fatalf("opens = %d after the retry window, want 2", bo.opens)
	}
	if r := s.report(); r.Board == nil || r.Board.Chip != "Nuvoton NCT6798D" {
		t.Fatalf("board = %+v after a successful reopen", r.Board)
	}
}

// TestSamplerStopClosesBoardSensor: the board handle is released on shutdown
// alongside the CPU one.
func TestSamplerStopClosesBoardSensor(t *testing.T) {
	b := &fakeBoard{script: []boardResult{{reading: nuvotonReading()}}}
	o := &opener{queue: []*fakeSensor{{script: []readResult{{c: 58}}}}}
	bo := &boardOpener{queue: []*fakeBoard{b}}
	s := startSampler(time.Hour, quietLog(), o.open, bo.open)
	for i := 0; i < 100 && b.reads == 0; i++ {
		time.Sleep(time.Millisecond)
	}
	s.Stop()
	if !b.closed {
		t.Fatal("Stop did not close the board sensor")
	}
}

// TestSamplerPublishesPackageWatts: power rides the same tick and the same
// SampledAt as the temperature, so a reader applying one freshness window
// gets a consistent pair.
func TestSamplerPublishesPackageWatts(t *testing.T) {
	fs := &fakeSensor{script: []readResult{{c: 58}}, watts: 140, powerOK: true}
	o := &opener{queue: []*fakeSensor{fs}}
	now := time.Date(2026, 9, 20, 16, 0, 0, 0, time.UTC)
	s := newSampler(tickInterval, tickRetry, quietLog(), o.open, func() time.Time { return now })

	s.tick(&samplerState{})
	r := s.report()
	if r.CPU == nil {
		t.Fatalf("no CPU reading: %+v", r)
	}
	if r.CPU.PackageWatts != 140 {
		t.Fatalf("package_watts = %v, want 140", r.CPU.PackageWatts)
	}
	if !r.CPU.SampledAt.Equal(now.UTC()) {
		t.Fatalf("SampledAt = %v, want the tick's own time", r.CPU.SampledAt)
	}
	if fs.powerCalls != 1 {
		t.Fatalf("power sampled %d times for one tick", fs.powerCalls)
	}

	watts, ok := r.CPUPackageWatts(now, time.Minute)
	if !ok || watts != 140 {
		t.Fatalf("CPUPackageWatts = %v, %v; want 140, true", watts, ok)
	}
}

// TestSamplerKeepsTheTemperatureWithoutPower is the failure mode worth
// guarding: this helper exists for the temperature, and a part whose energy
// counter it cannot read must not lose the reading every consumer depends on.
func TestSamplerKeepsTheTemperatureWithoutPower(t *testing.T) {
	fs := &fakeSensor{script: []readResult{{c: 58}}} // powerOK false: no counter
	o := &opener{queue: []*fakeSensor{fs}}
	now := time.Date(2026, 9, 20, 16, 0, 0, 0, time.UTC)
	s := newSampler(tickInterval, tickRetry, quietLog(), o.open, func() time.Time { return now })

	s.tick(&samplerState{})
	r := s.report()
	if r.CPU == nil || r.CPU.PackageCelsius != 58 {
		t.Fatalf("temperature lost with the power: %+v", r.CPU)
	}
	if r.CPU.PackageWatts != 0 {
		t.Fatalf("package_watts = %v, want zero (omitted)", r.CPU.PackageWatts)
	}
	if r.Error != "" {
		t.Fatalf("a missing wattage was reported as an error: %q", r.Error)
	}
	if _, ok := r.CPUPackageWatts(now, time.Minute); ok {
		t.Fatal("CPUPackageWatts reported a figure for a report that carries none")
	}
}
