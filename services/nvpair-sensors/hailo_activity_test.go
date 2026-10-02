// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestIODutyNeedsHalfAWindow(t *testing.T) {
	d := newIODuty(10, 4)
	for i := 0; i < 4; i++ {
		d.observe(100, hailoSlot)
	}
	if _, ok := d.busyPercent(); ok {
		t.Fatal("4 of 10 slots observed: want no figure yet")
	}
	d.observe(100, hailoSlot)
	if pct, ok := d.busyPercent(); !ok || pct != 100 {
		t.Fatalf("5 busy of 5 observed: got %d %v, want 100 true", pct, ok)
	}
}

func TestIODutyFloorAndRing(t *testing.T) {
	d := newIODuty(4, 4)
	for _, ops := range []uint64{3, 4, 0, 50} { // below floor, at floor, idle, busy
		d.observe(ops, hailoSlot)
	}
	if pct, _ := d.busyPercent(); pct != 50 {
		t.Fatalf("got %d%%, want 50%%", pct)
	}
	// The ring overwrites the oldest slots: four more idle slots read 0.
	for i := 0; i < 4; i++ {
		d.observe(0, hailoSlot)
	}
	if pct, _ := d.busyPercent(); pct != 0 {
		t.Fatalf("after a window of idle slots: got %d%%, want 0%%", pct)
	}
}

type fakeProc struct {
	ops    uint64
	err    error
	closed int
}

func (f *fakeProc) proc(pid uint32, name string) hailoProc {
	return hailoProc{pid: pid, name: name,
		ops:   func() (uint64, error) { return f.ops, f.err },
		close: func() { f.closed++ }}
}

// scanOf turns a candidate list into a successful scan in which exactly those
// pids are alive.
func scanOf(procs ...hailoProc) hailoScan {
	alive := map[uint32]bool{}
	for _, p := range procs {
		alive[p.pid] = true
	}
	return hailoScan{found: procs, alive: alive, ok: true}
}

func newTestMonitor(scan func() []hailoProc) *hailoMonitor {
	return newHailoMonitor(func() hailoScan { return scanOf(scan()...) },
		func() time.Time { return time.Unix(1_790_000_000, 0) },
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// A saturated process (44 ops per slot, as measured on a Hailo-8 running
// yolov8s) reads 100 %; the same process idle reads 0 %.
func TestHailoMonitorBusyAndIdle(t *testing.T) {
	p := &fakeProc{}
	m := newTestMonitor(func() []hailoProc { return []hailoProc{p.proc(42, "hailortcli.exe")} })
	// The first slot only primes the new process's baseline (so it reads idle);
	// run past it to the next publish, when the window is all busy slots.
	for i := 0; i < hailoWindowSlots+hailoPublishEvery; i++ {
		p.ops += 44
		m.slot()
	}
	got := m.current()
	if got == nil || got.BusyPercent != 100 || got.Source != "io-activity" || len(got.Processes) != 1 || got.Processes[0] != "hailortcli.exe" {
		t.Fatalf("saturated: got %+v", got)
	}
	for i := 0; i < hailoWindowSlots; i++ {
		m.slot() // counter does not move
	}
	if got := m.current(); got.BusyPercent != 0 {
		t.Fatalf("idle: got %d%%, want 0%%", got.BusyPercent)
	}
}

// The first read of a process only primes its baseline: a process found with
// a large cumulative count does not register a burst of past traffic.
func TestHailoMonitorPrimesBaseline(t *testing.T) {
	p := &fakeProc{ops: 1_000_000}
	m := newTestMonitor(func() []hailoProc { return []hailoProc{p.proc(7, "app.exe")} })
	for i := 0; i < hailoWindowSlots; i++ {
		m.slot()
	}
	if got := m.current(); got == nil || got.BusyPercent != 0 {
		t.Fatalf("got %+v, want 0%%", got)
	}
}

// A process whose counter read fails (it exited) is dropped and its handle
// closed; a rescan that no longer lists a process drops it too, and a process
// listed again keeps its tracked handle (the duplicate from the scan is closed).
func TestHailoMonitorProcessLifecycle(t *testing.T) {
	a, b := &fakeProc{}, &fakeProc{}
	var listed []hailoProc
	m := newTestMonitor(func() []hailoProc { return listed })
	listed = []hailoProc{a.proc(1, "a.exe"), b.proc(2, "b.exe")}
	m.slot()
	if len(m.procs) != 2 {
		t.Fatalf("watching %d, want 2", len(m.procs))
	}
	a.err = errors.New("exited")
	m.slot()
	if _, ok := m.procs[1]; ok || a.closed != 1 {
		t.Fatalf("exited process still watched (closed %d)", a.closed)
	}
	// Next rescan lists only b again: b's fresh scan handle is closed, b kept.
	listed = []hailoProc{b.proc(2, "b.exe")}
	for m.ticks%hailoRescanEvery != 0 {
		m.slot()
	}
	m.slot()
	if _, ok := m.procs[2]; !ok || b.closed != 1 {
		t.Fatalf("b dropped or its duplicate handle leaked (closed %d)", b.closed)
	}
	// A rescan that lists nothing drops b.
	listed = nil
	for m.ticks%hailoRescanEvery != 0 {
		m.slot()
	}
	m.slot()
	if len(m.procs) != 0 || b.closed != 2 {
		t.Fatalf("watching %d after an empty scan (b closed %d)", len(m.procs), b.closed)
	}
}

// A late tick (a long slot) is weighted by the time it spanned.
func TestIODutyWeightsSlotsByLength(t *testing.T) {
	d := newIODuty(4, 1)
	d.observe(5, 30*time.Millisecond) // busy, 30 ms
	d.observe(0, 10*time.Millisecond)
	d.observe(0, 10*time.Millisecond)
	d.observe(0, 10*time.Millisecond)
	if pct, _ := d.busyPercent(); pct != 50 {
		t.Fatalf("30 ms busy of 60 ms: got %d%%, want 50%%", pct)
	}
}

// Frames every ~100 ms (10 FPS of a model that keeps the device busy for
// part of one slot) leave every other 50 ms slot call-free: ~50 %.
func TestHailoMonitorPartialLoad(t *testing.T) {
	p := &fakeProc{}
	clock := time.Unix(1_790_000_000, 0)
	m := newHailoMonitor(func() hailoScan { return scanOf(p.proc(9, "hailortcli.exe")) },
		func() time.Time { clock = clock.Add(hailoSlot); return clock },
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	for i := 0; i < hailoWindowSlots+hailoPublishEvery; i++ {
		if i%2 == 0 {
			p.ops += 16
		}
		m.slot()
	}
	got := m.current()
	if got == nil || got.BusyPercent < 48 || got.BusyPercent > 52 {
		t.Fatalf("got %+v, want ~50%%", got)
	}
}

// A scan that fails, or that misses a live process (a transient open or
// module-list failure), keeps the watched set: a saturated device must not
// read as idle for a window because one scan came back short.
func TestHailoMonitorKeepsProcessesOnShortScan(t *testing.T) {
	p := &fakeProc{}
	var sc hailoScan
	m := newHailoMonitor(func() hailoScan { return sc },
		func() time.Time { return time.Unix(1_790_000_000, 0) },
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	sc = scanOf(p.proc(5, "hailortcli.exe"))
	m.slot()
	sc = hailoScan{ok: false} // EnumProcesses failed
	m.rescan()
	sc = hailoScan{alive: map[uint32]bool{5: true}, ok: true} // alive, but not re-found
	m.rescan()
	if _, ok := m.procs[5]; !ok {
		t.Fatal("a live process was dropped by a short scan")
	}
	sc = hailoScan{alive: map[uint32]bool{}, ok: true} // gone
	m.rescan()
	if _, ok := m.procs[5]; ok || p.closed != 1 {
		t.Fatalf("a gone process is still watched (closed %d)", p.closed)
	}
}

// A long slot needs proportionally more operations to count as busy, so a few
// stray calls spread over a late tick do not mark its whole length busy.
func TestIODutyScalesFloorByLength(t *testing.T) {
	d := newIODuty(2, 4)
	d.observe(6, 3*hailoSlot) // 6 ops over 3 slots' time: below 12, idle
	d.observe(4, hailoSlot)   // busy
	if pct, _ := d.busyPercent(); pct != 25 {
		t.Fatalf("got %d%%, want 25%% (1 busy slot-length of 4)", pct)
	}
}

func TestProcNamesDedupedAndCapped(t *testing.T) {
	m := newTestMonitor(func() []hailoProc { return nil })
	for i := 0; i < hailoMaxNames+10; i++ {
		f := &fakeProc{}
		name := "a.exe"
		if i >= 2 {
			name = fmt.Sprintf("p%02d.exe", i)
		}
		m.procs[uint32(i+1)] = &trackedProc{hailoProc: f.proc(uint32(i+1), name)}
	}
	names := m.procNames()
	if len(names) != hailoMaxNames || names[0] != "a.exe" || names[1] == "a.exe" {
		t.Fatalf("got %v", names)
	}
}

func TestIsPAIRProcess(t *testing.T) {
	for name, want := range map[string]bool{
		"nvpair-node-info.exe": true, "NVPAIR-Sensors.exe": true,
		"hailortcli.exe": false, "python.exe": false, "": false,
	} {
		if got := isPAIRProcess(name); got != want {
			t.Errorf("isPAIRProcess(%q) = %v, want %v", name, got, want)
		}
	}
}
