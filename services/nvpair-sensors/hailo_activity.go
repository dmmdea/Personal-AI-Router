// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"log/slog"
	"math"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"nvpair-shared/hostsensors"
)

// Hailo activity estimate (hostsensors.HailoActivity says what it is and why it
// is an estimate). This file is the platform-neutral core: the per-slot duty
// arithmetic and the bookkeeping of the processes being watched. The Windows
// side (hailo_activity_windows.go) supplies the process scan and the counter
// reads; both are injected, so everything here is testable without HailoRT.

const (
	// hailoSlot is the sampling slot. A yolov8s frame on a Hailo-8 is ~18 ms of
	// device time (~54 FPS at saturation) and ~16 DeviceIoControl calls issued
	// in bursts at its start and end. Calibrated on an OptiPlex 7060 (HailoRT
	// 4.24, hailortcli run2 at unlimited / 27 / 10 FPS against ~100 / 50 / 18 %
	// true duty): 50 ms slots read 99 / 99 / 55 %; 10 ms slots read 73 / 64 /
	// 36 %, because a saturated frame leaves call-free 10 ms gaps between its
	// bursts. 50 ms is exact at idle and at saturation and reads a partial load
	// high; finer slots trade the saturated reading away without fixing the
	// partial one. Operation counts cannot give true duty across loads (calls
	// per frame and frame length are both per model), so this is published as
	// an activity estimate, below the inference process's own duty file.
	hailoSlot = 50 * time.Millisecond
	// hailoWindowSlots is the averaging window: 100 slots = 5 s, the length of
	// node-info's Hailo sampler tick.
	hailoWindowSlots = 100
	// hailoSlotFloor is the fewest operations that mark a slot busy. A
	// saturated device drives ~44 ops per 50 ms; an idle process holding the
	// library does 0-7 stray ops per 250 ms.
	hailoSlotFloor = 4
	// hailoRescanEvery is how often the process list is rescanned, in slots (5 s).
	hailoRescanEvery = 100
	// hailoPublishEvery is how often an estimate is published, in slots (1 s).
	hailoPublishEvery = 20
)

// ioDuty is a ring of slots: how long each lasted and whether it was busy.
// Slots are weighted by their measured length, so a late timer tick (a long
// slot) counts for what it spanned rather than as one ordinary slot.
type ioDuty struct {
	floor  uint64
	busy   []bool
	length []time.Duration
	next   int
	filled int
}

func newIODuty(windowSlots int, floor uint64) *ioDuty {
	return &ioDuty{floor: floor, busy: make([]bool, windowSlots), length: make([]time.Duration, windowSlots)}
}

// observe records one slot: its operation count and how long it lasted.
func (d *ioDuty) observe(ops uint64, length time.Duration) {
	if length <= 0 {
		length = hailoSlot
	}
	// The floor is per nominal slot; a long slot (a late tick, the rescan slot)
	// needs proportionally more operations, so a few stray calls spread over
	// it do not mark its whole length busy.
	need := d.floor
	if length > hailoSlot {
		need = uint64(math.Ceil(float64(d.floor) * float64(length) / float64(hailoSlot)))
	}
	d.busy[d.next] = ops >= need
	d.length[d.next] = length
	d.next = (d.next + 1) % len(d.busy)
	if d.filled < len(d.busy) {
		d.filled++
	}
}

// busyPercent is the busy share of the observed time, rounded. ok is false
// until half a window has been observed, so a fresh start does not publish a
// figure built on a handful of slots.
func (d *ioDuty) busyPercent() (pct uint32, ok bool) {
	if d.filled < len(d.busy)/2 {
		return 0, false
	}
	var busy, total time.Duration
	for i := 0; i < d.filled; i++ {
		total += d.length[i]
		if d.busy[i] {
			busy += d.length[i]
		}
	}
	if total <= 0 {
		return 0, false
	}
	return uint32(math.Round(float64(busy) * 100 / float64(total))), true
}

// hailoProc is one candidate process the scan found: a process that has the
// HailoRT library loaded and is not one of PAIR's own.
type hailoProc struct {
	pid  uint32
	name string
	// ops returns the process's cumulative "other operations" count; an error
	// means the process is gone (exited, or the handle no longer answers).
	ops func() (uint64, error)
	// close releases whatever ops holds open.
	close func()
}

type trackedProc struct {
	hailoProc
	last   uint64
	primed bool
}

// hailoMonitor owns the watched processes and the duty ring. One goroutine
// drives it (slot); readers take the latest estimate atomically.
// hailoScan is one scan's result: the candidate processes found, every pid
// alive at scan time, and whether the scan itself worked. A failed scan keeps
// the watched set as it is; a process is dropped only when its own counter
// read fails or its pid is no longer alive — never because a transient open
// or module-list failure kept it out of one scan's candidates.
type hailoScan struct {
	found []hailoProc
	alive map[uint32]bool
	ok    bool
}

type hailoMonitor struct {
	scan func() hailoScan
	now  func() time.Time
	log  *slog.Logger

	procs  map[uint32]*trackedProc
	duty   *ioDuty
	ticks  int
	last   time.Time
	latest atomic.Pointer[hostsensors.HailoActivity]
	names  string
}

func newHailoMonitor(scan func() hailoScan, now func() time.Time, log *slog.Logger) *hailoMonitor {
	return &hailoMonitor{
		scan:  scan,
		now:   now,
		log:   log,
		procs: map[uint32]*trackedProc{},
		duty:  newIODuty(hailoWindowSlots, hailoSlotFloor),
	}
}

// slot performs one sampling step: rescan when due, read every watched
// process's counter, record the slot, publish when due.
func (m *hailoMonitor) slot() {
	if m.ticks%hailoRescanEvery == 0 {
		m.rescan()
	}
	var total uint64
	for pid, p := range m.procs {
		v, err := p.ops()
		if err != nil {
			p.close()
			delete(m.procs, pid)
			continue
		}
		if p.primed && v >= p.last {
			total += v - p.last
		}
		p.last, p.primed = v, true
	}
	now := m.now()
	var length time.Duration
	if !m.last.IsZero() {
		length = now.Sub(m.last)
	}
	m.last = now
	m.duty.observe(total, length)
	m.ticks++
	if m.ticks%hailoPublishEvery == 0 {
		m.publish()
	}
}

// rescan adds newly found processes and drops the ones the scan no longer
// lists. A pid already watched keeps its open handle and baseline.
func (m *hailoMonitor) rescan() {
	sc := m.scan()
	for _, p := range sc.found {
		if _, ok := m.procs[p.pid]; ok {
			p.close()
			continue
		}
		m.procs[p.pid] = &trackedProc{hailoProc: p}
	}
	if sc.ok {
		for pid, p := range m.procs {
			if !sc.alive[pid] {
				p.close()
				delete(m.procs, pid)
			}
		}
	}
	names := m.procNames()
	if joined := strings.Join(names, ","); joined != m.names {
		m.log.Info("Hailo activity: watched processes", "processes", joined)
		m.names = joined
	}
}

// hailoMaxNames bounds the process list in a report (the pipe report is
// capped at 64 KiB and a reader that cannot decode it loses the CPU reading).
const hailoMaxNames = 16

func (m *hailoMonitor) procNames() []string {
	seen := map[string]bool{}
	var names []string
	for _, p := range m.procs {
		if !seen[p.name] {
			seen[p.name] = true
			names = append(names, p.name)
		}
	}
	sort.Strings(names)
	if len(names) > hailoMaxNames {
		names = names[:hailoMaxNames]
	}
	return names
}

func (m *hailoMonitor) publish() {
	pct, ok := m.duty.busyPercent()
	if !ok {
		return
	}
	m.latest.Store(&hostsensors.HailoActivity{
		BusyPercent: pct,
		Processes:   m.procNames(),
		SlotMS:      uint32(hailoSlot / time.Millisecond),
		WindowMS:    uint32(hailoSlot/time.Millisecond) * hailoWindowSlots,
		Source:      "io-activity",
		SampledAt:   m.now().UTC(),
	})
}

// current returns the latest estimate, nil before the first.
func (m *hailoMonitor) current() *hostsensors.HailoActivity {
	if m == nil {
		return nil
	}
	return m.latest.Load()
}

// closeAll releases every watched process.
func (m *hailoMonitor) closeAll() {
	for pid, p := range m.procs {
		p.close()
		delete(m.procs, pid)
	}
}

// isPAIRProcess reports whether an image name is one of PAIR's own workers.
// They are excluded because node-info loads libhailort.dll to read the chip
// temperature, and its unrelated pollers burst ~690 operations every 1.5 s.
func isPAIRProcess(image string) bool {
	return strings.HasPrefix(strings.ToLower(image), "nvpair-")
}
