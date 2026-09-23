// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"testing"
	"time"
)

// activityJSON renders a schema-1 file for the Hailo writer.
func activityJSON(pid int, started, updated, busy int64, inflight int) []byte {
	return []byte(fmt.Sprintf(
		`{"schema":1,"device":"hailo-8l","pid":%d,"started_ms":%d,"updated_ms":%d,"busy_ms":%d,"inflight":%d}`,
		pid, started, updated, busy, inflight))
}

func TestParseAccelActivity(t *testing.T) {
	good := activityJSON(4242, 1_700_000_000_000, 1_700_000_005_000, 1234, 1)
	got, err := parseAccelActivity(good, accelActivityDeviceHailo8L)
	if err != nil {
		t.Fatalf("good file: %v", err)
	}
	want := accelActivity{PID: 4242, StartedMS: 1_700_000_000_000, UpdatedMS: 1_700_000_005_000, BusyMS: 1234, Inflight: 1}
	if got != want {
		t.Fatalf("parsed = %+v, want %+v", got, want)
	}

	// A byte-order mark in front of the JSON is tolerated.
	bom := append([]byte("\xef\xbb\xbf"), good...)
	if got, err := parseAccelActivity(bom, accelActivityDeviceHailo8L); err != nil || got != want {
		t.Fatalf("BOM-prefixed file = (%+v, %v), want %+v", got, err, want)
	}

	// Millisecond figures written as JSON floats are accepted.
	floaty := []byte(`{"schema":1,"device":"hailo-8l","pid":7,"started_ms":1000.0,"updated_ms":2000.5,"busy_ms":10.25,"inflight":0}`)
	if got, err := parseAccelActivity(floaty, accelActivityDeviceHailo8L); err != nil || got.BusyMS != 10.25 || got.UpdatedMS != 2000.5 {
		t.Fatalf("float millisecond fields = (%+v, %v)", got, err)
	}

	bad := map[string]string{
		"not JSON":          `{"schema":1,`,
		"empty":             ``,
		"JSON array":        `[1,2]`,
		"wrong schema":      `{"schema":2,"device":"hailo-8l","pid":1,"started_ms":1,"updated_ms":2,"busy_ms":0,"inflight":0}`,
		"missing schema":    `{"device":"hailo-8l","pid":1,"started_ms":1,"updated_ms":2,"busy_ms":0,"inflight":0}`,
		"wrong device":      `{"schema":1,"device":"hailo-8","pid":1,"started_ms":1,"updated_ms":2,"busy_ms":0,"inflight":0}`,
		"device case":       `{"schema":1,"device":"Hailo-8L","pid":1,"started_ms":1,"updated_ms":2,"busy_ms":0,"inflight":0}`,
		"missing device":    `{"schema":1,"pid":1,"started_ms":1,"updated_ms":2,"busy_ms":0,"inflight":0}`,
		"zero pid":          `{"schema":1,"device":"hailo-8l","pid":0,"started_ms":1,"updated_ms":2,"busy_ms":0,"inflight":0}`,
		"pid overflow":      `{"schema":1,"device":"hailo-8l","pid":4294967296,"started_ms":1,"updated_ms":2,"busy_ms":0,"inflight":0}`,
		"float pid":         `{"schema":1,"device":"hailo-8l","pid":1.5,"started_ms":1,"updated_ms":2,"busy_ms":0,"inflight":0}`,
		"missing busy":      `{"schema":1,"device":"hailo-8l","pid":1,"started_ms":1,"updated_ms":2,"inflight":0}`,
		"negative busy":     `{"schema":1,"device":"hailo-8l","pid":1,"started_ms":1,"updated_ms":2,"busy_ms":-1,"inflight":0}`,
		"missing updated":   `{"schema":1,"device":"hailo-8l","pid":1,"started_ms":1,"busy_ms":0,"inflight":0}`,
		"zero started":      `{"schema":1,"device":"hailo-8l","pid":1,"started_ms":0,"updated_ms":2,"busy_ms":0,"inflight":0}`,
		"string busy":       `{"schema":1,"device":"hailo-8l","pid":1,"started_ms":1,"updated_ms":2,"busy_ms":"5","inflight":0}`,
		"missing inflight":  `{"schema":1,"device":"hailo-8l","pid":1,"started_ms":1,"updated_ms":2,"busy_ms":0}`,
		"negative inflight": `{"schema":1,"device":"hailo-8l","pid":1,"started_ms":1,"updated_ms":2,"busy_ms":0,"inflight":-1}`,
	}
	for name, body := range bad {
		if _, err := parseAccelActivity([]byte(body), accelActivityDeviceHailo8L); !errors.Is(err, errAccelActivityInvalid) {
			t.Errorf("%s: err = %v, want errAccelActivityInvalid", name, err)
		}
	}
}

func TestAccelActivityPercent(t *testing.T) {
	sample := func(updated, busy float64) accelActivity {
		return accelActivity{PID: 1, StartedMS: 1, UpdatedMS: updated, BusyMS: busy}
	}
	cases := []struct {
		name      string
		prev, cur accelActivity
		want      uint32
	}{
		{"idle", sample(1000, 50), sample(6000, 50), 0},
		{"half busy", sample(1000, 0), sample(6000, 2500), 50},
		{"rounds half up", sample(0, 0), sample(1000, 125), 13},
		{"rounds down", sample(0, 0), sample(1000, 124), 12},
		{"fully busy", sample(1000, 0), sample(6000, 5000), 100},
		// busy_ms includes the in-flight portion up to updated_ms; timer
		// jitter between the two figures can push the ratio a hair past 1.
		{"clamped above 100", sample(1000, 0), sample(6000, 5400), 100},
		{"clamped below 0", sample(1000, 900), sample(6000, 100), 0},
		{"no elapsed time", sample(1000, 0), sample(1000, 10), 0},
	}
	for _, c := range cases {
		if got := accelActivityPercent(c.prev, c.cur); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
}

// activityClock is a reader clock anchored to a fixed epoch so updated_ms
// values can be written as offsets from it.
var activityEpoch = time.UnixMilli(1_700_000_000_000)

func at(ms int64) time.Time { return activityEpoch.Add(time.Duration(ms) * time.Millisecond) }

func writerSample(pid uint32, started, updated, busy int64) accelActivity {
	return accelActivity{
		PID:       pid,
		StartedMS: float64(activityEpoch.UnixMilli() + started),
		UpdatedMS: float64(activityEpoch.UnixMilli() + updated),
		BusyMS:    float64(busy),
	}
}

func alwaysAlive(accelActivity) bool { return true }
func neverAlive(accelActivity) bool  { return false }

// TestAccelActivityTrackerStates is the reader-rule state table: every
// combination of file presence, freshness and writer liveness maps to exactly
// one published outcome.
func TestAccelActivityTrackerStates(t *testing.T) {
	absentErr := fmt.Errorf("open hailo.json: %w", fs.ErrNotExist)
	badErr := fmt.Errorf("%w: schema 2", errAccelActivityInvalid)

	cases := []struct {
		name      string
		sample    accelActivity
		err       error
		now       time.Time
		alive     func(accelActivity) bool
		wantKnown bool
		wantPct   uint32
		wantState accelActivityState
	}{
		{"absent file", accelActivity{}, absentErr, at(0), alwaysAlive, false, 0, activityAbsent},
		{"unusable file", accelActivity{}, badErr, at(0), alwaysAlive, false, 0, activityInvalid},
		{"fresh, first sample", writerSample(9, 0, 5000, 100), nil, at(5500), alwaysAlive, false, 0, activityPending},
		{"stale, writer dead", writerSample(9, 0, 5000, 100), nil, at(8001), neverAlive, true, 0, activityWriterGone},
		{"stale, writer alive", writerSample(9, 0, 5000, 100), nil, at(8001), alwaysAlive, false, 0, activityWriterSilent},
		{"exactly 3 s old is fresh", writerSample(9, 0, 5000, 100), nil, at(8000), neverAlive, false, 0, activityPending},
		{"far future is stale", writerSample(9, 0, 5000, 100), nil, at(1000), neverAlive, true, 0, activityWriterGone},
	}
	for _, c := range cases {
		var tr accelActivityTracker
		pct, known, state := tr.observe(c.sample, c.err, c.now, c.alive)
		if state != c.wantState || known != c.wantKnown || pct != c.wantPct {
			t.Errorf("%s: (pct=%d known=%v state=%v), want (pct=%d known=%v state=%v)",
				c.name, pct, known, state, c.wantPct, c.wantKnown, c.wantState)
		}
	}
}

// TestAccelActivityTrackerLiveness pins that liveness is asked only for a
// stale sample: a fresh file is proof enough that its writer runs.
func TestAccelActivityTrackerLiveness(t *testing.T) {
	var tr accelActivityTracker
	asked := 0
	alive := func(accelActivity) bool { asked++; return true }
	tr.observe(writerSample(9, 0, 5000, 0), nil, at(5000), alive)
	tr.observe(writerSample(9, 0, 10000, 0), nil, at(10000), alive)
	if asked != 0 {
		t.Fatalf("liveness asked %d times for fresh samples, want 0", asked)
	}
	tr.observe(writerSample(9, 0, 10000, 0), nil, at(20000), alive)
	if asked != 1 {
		t.Fatalf("liveness asked %d times for one stale sample, want 1", asked)
	}
}

// TestAccelActivityTrackerSequence walks one writer's life through the
// tracker: baseline, deltas, a held value, a restart, and the exit.
func TestAccelActivityTrackerSequence(t *testing.T) {
	var tr accelActivityTracker
	step := func(label string, s accelActivity, now time.Time, alive func(accelActivity) bool,
		wantPct uint32, wantKnown bool, wantState accelActivityState,
	) {
		t.Helper()
		pct, known, state := tr.observe(s, nil, now, alive)
		if pct != wantPct || known != wantKnown || state != wantState {
			t.Fatalf("%s: (pct=%d known=%v state=%v), want (pct=%d known=%v state=%v)",
				label, pct, known, state, wantPct, wantKnown, wantState)
		}
	}

	step("baseline", writerSample(9, 0, 5000, 1000), at(5200), alwaysAlive, 0, false, activityPending)
	step("40% over 5 s", writerSample(9, 0, 10000, 3000), at(10200), alwaysAlive, 40, true, activityFresh)
	// The same updated_ms again: no new data, the last figure is held while
	// the sample is still fresh...
	step("held", writerSample(9, 0, 10000, 3000), at(12000), alwaysAlive, 40, true, activityFresh)
	// ...and the next distinct sample is measured against the held baseline,
	// not against the repeated read.
	step("idle 5 s", writerSample(9, 0, 15000, 3000), at(15100), alwaysAlive, 0, true, activityFresh)
	// A fresh idle writer publishes a real 0.
	step("still idle", writerSample(9, 0, 20000, 3000), at(20100), alwaysAlive, 0, true, activityFresh)

	// Restart: a new started_ms (same pid) resets the baseline, because the
	// new run's busy_ms starts over and a delta against the old run would be
	// negative or meaningless.
	step("restart baseline", writerSample(9, 21000, 25000, 10), at(25100), alwaysAlive, 0, false, activityPending)
	step("restart delta", writerSample(9, 21000, 30000, 5010), at(30100), alwaysAlive, 100, true, activityFresh)

	// A different pid is a different writer too.
	step("new pid baseline", writerSample(10, 21000, 35000, 0), at(35100), alwaysAlive, 0, false, activityPending)
	step("new pid delta", writerSample(10, 21000, 40000, 1250), at(40100), alwaysAlive, 25, true, activityFresh)

	// Counters that go backwards within one writer: start over.
	step("busy went backwards", writerSample(10, 21000, 45000, 100), at(45100), alwaysAlive, 0, false, activityPending)
	step("recovered", writerSample(10, 21000, 50000, 600), at(50100), alwaysAlive, 10, true, activityFresh)

	// The writer exits: its last copy goes stale and the pid is gone.
	step("writer exited", writerSample(10, 21000, 50000, 600), at(60000), neverAlive, 0, true, activityWriterGone)
}

// TestAccelActivityTrackerErrorKeepsBaseline pins that a transient read
// failure between two samples of one writer does not discard the baseline:
// the delta across the gap is still a delta of that writer's own counters.
func TestAccelActivityTrackerErrorKeepsBaseline(t *testing.T) {
	var tr accelActivityTracker
	tr.observe(writerSample(9, 0, 5000, 0), nil, at(5000), alwaysAlive)
	if _, known, state := tr.observe(accelActivity{}, errors.New("sharing violation"), at(7500), alwaysAlive); known || state != activityInvalid {
		t.Fatalf("read error: known=%v state=%v", known, state)
	}
	pct, known, state := tr.observe(writerSample(9, 0, 10000, 2500), nil, at(10000), alwaysAlive)
	if !known || pct != 50 || state != activityFresh {
		t.Fatalf("after the error: (pct=%d known=%v state=%v), want 50 from the kept baseline", pct, known, state)
	}
}
