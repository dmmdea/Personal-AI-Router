// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"time"
)

// Accelerator activity file: the busy figure for an accelerator whose own
// runtime exposes no busy counter (a Hailo module on Windows, see
// accel_windows.go). The process that runs inference on the device tracks how
// long it has had at least one device call in flight and publishes that as a
// small JSON file; node-info reads it on the device sampler's tick and turns
// two successive samples into utilization_percent.
//
// This file holds only the platform-neutral part — the parser and the delta
// state machine — so it is unit-tested on every platform. Where the file lives,
// how it is opened and how writer liveness is checked are platform concerns
// (accel_activity_windows.go).
//
// Contract, schema 1 (the writer implements exactly this):
//
//	{"schema":1,"device":"hailo-8l","pid":<int>,"started_ms":<epoch ms>,
//	 "updated_ms":<epoch ms>,"busy_ms":<cumulative ms>,"inflight":<int>}
//
// started_ms is when the writer's tracker started, updated_ms when this copy
// was written, busy_ms the cumulative milliseconds with at least one device
// call in flight (including the in-flight portion up to updated_ms). The
// writer replaces the file (temp + rename) about every 500 ms while it lives
// and writes a last copy with inflight 0 when it exits.

const (
	// accelActivitySchema is the only schema this reader understands.
	accelActivitySchema = 1

	// accelActivityDeviceHailo8L is the device name the Hailo writer uses.
	accelActivityDeviceHailo8L = "hailo-8l"

	// accelActivityFreshWindow is how far updated_ms may sit from the
	// reader's clock for the sample to count as current. The writer refreshes
	// every 500 ms, so 3 s is six missed writes.
	accelActivityFreshWindow = 3 * time.Second

	// accelActivityMaxBytes bounds a read. A schema-1 file is under 200
	// bytes; anything this large is not one.
	accelActivityMaxBytes = 64 << 10
)

// accelActivity is one parsed copy of the activity file. The millisecond
// fields are float64 so a writer that emits 1234.0 is not rejected; epoch
// milliseconds are exact in a float64 for the next quarter-million years.
type accelActivity struct {
	PID       uint32
	StartedMS float64
	UpdatedMS float64
	BusyMS    float64
	Inflight  int
}

// sameWriter reports whether two samples come from one writer run: the same
// process and the same tracker start. A restarted writer (or a reused pid)
// gets a fresh baseline, because its busy_ms starts over.
func (a accelActivity) sameWriter(b accelActivity) bool {
	return a.PID == b.PID && a.StartedMS == b.StartedMS
}

// updatedAt is updated_ms as a time.Time.
func (a accelActivity) updatedAt() time.Time {
	return time.UnixMilli(int64(a.UpdatedMS))
}

// errAccelActivityInvalid wraps every reason a file is not a usable schema-1
// sample for the expected device.
var errAccelActivityInvalid = errors.New("invalid accelerator activity file")

// accelActivityWire is the on-disk shape. Pointers tell a missing field from
// a zero one: a file without busy_ms is malformed, not idle.
type accelActivityWire struct {
	Schema    *int     `json:"schema"`
	Device    *string  `json:"device"`
	PID       *int64   `json:"pid"`
	StartedMS *float64 `json:"started_ms"`
	UpdatedMS *float64 `json:"updated_ms"`
	BusyMS    *float64 `json:"busy_ms"`
	Inflight  *int     `json:"inflight"`
}

// parseAccelActivity decodes one copy of the file and checks it against the
// contract: schema 1, the expected device, and every field present and in
// range. A UTF-8 byte-order mark is tolerated, since some Windows writers add
// one.
func parseAccelActivity(data []byte, device string) (accelActivity, error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	var w accelActivityWire
	if err := json.Unmarshal(data, &w); err != nil {
		return accelActivity{}, fmt.Errorf("%w: %v", errAccelActivityInvalid, err)
	}
	invalid := func(format string, args ...any) (accelActivity, error) {
		return accelActivity{}, fmt.Errorf("%w: "+format, append([]any{errAccelActivityInvalid}, args...)...)
	}
	switch {
	case w.Schema == nil:
		return invalid("schema missing")
	case *w.Schema != accelActivitySchema:
		return invalid("schema %d, want %d", *w.Schema, accelActivitySchema)
	case w.Device == nil:
		return invalid("device missing")
	case *w.Device != device:
		return invalid("device %q, want %q", *w.Device, device)
	case w.PID == nil || *w.PID <= 0 || *w.PID > math.MaxUint32:
		return invalid("pid missing or out of range")
	case !validMS(w.StartedMS) || *w.StartedMS <= 0:
		return invalid("started_ms missing or out of range")
	case !validMS(w.UpdatedMS) || *w.UpdatedMS <= 0:
		return invalid("updated_ms missing or out of range")
	case !validMS(w.BusyMS):
		return invalid("busy_ms missing or out of range")
	case w.Inflight == nil || *w.Inflight < 0:
		return invalid("inflight missing or negative")
	}
	return accelActivity{
		PID:       uint32(*w.PID),
		StartedMS: *w.StartedMS,
		UpdatedMS: *w.UpdatedMS,
		BusyMS:    *w.BusyMS,
		Inflight:  *w.Inflight,
	}, nil
}

// validMS reports a present, finite, non-negative millisecond figure.
func validMS(v *float64) bool {
	return v != nil && !math.IsNaN(*v) && !math.IsInf(*v, 0) && *v >= 0
}

// accelActivityState is what one observation concluded. The sampler logs a
// line only when it changes.
type accelActivityState int

const (
	// activityAbsent: no file. The normal state of a host where nothing
	// runs inference on the device, and of one whose runtime predates the
	// file. Utilization unavailable.
	activityAbsent accelActivityState = iota
	// activityInvalid: the file exists but could not be read, is not JSON,
	// or is not a schema-1 file for this device. Utilization unavailable.
	activityInvalid
	// activityPending: a current sample from a writer, but no earlier sample
	// of the same writer to take a delta against yet. Utilization
	// unavailable until the next distinct sample.
	activityPending
	// activityFresh: a current sample and a delta. Utilization published.
	activityFresh
	// activityWriterGone: the sample is stale and its writer is no longer
	// running — the inference process exited, so the device is idle.
	// Utilization published as 0.
	activityWriterGone
	// activityWriterSilent: the sample is stale but its writer is still
	// running, so it has stopped reporting and nothing is known about the
	// device. Utilization unavailable.
	activityWriterSilent
)

func (s accelActivityState) String() string {
	switch s {
	case activityAbsent:
		return "absent"
	case activityInvalid:
		return "invalid"
	case activityPending:
		return "pending"
	case activityFresh:
		return "fresh"
	case activityWriterGone:
		return "writer-gone"
	case activityWriterSilent:
		return "writer-silent"
	}
	return fmt.Sprintf("state(%d)", int(s))
}

// accelActivityTracker turns successive reads of the file into a
// utilization figure. It keeps the last distinct sample of the current
// writer as the delta baseline and the last computed percentage, which is
// held while the writer's sample stays current but unchanged.
//
// Not safe for concurrent use: one sampler goroutine owns it.
type accelActivityTracker struct {
	base    accelActivity
	hasBase bool
	pct     uint32
	hasPct  bool
}

// observe folds one read into the tracker. sample and readErr are the result
// of reading and parsing the file (readErr matches fs.ErrNotExist for an
// absent file, anything else is an unusable one); now is the reader's clock;
// writerAlive answers whether the sample's writer process is still running,
// and is consulted only for a stale sample.
//
// The returned pct is meaningful only when known is true.
func (t *accelActivityTracker) observe(sample accelActivity, readErr error, now time.Time,
	writerAlive func(accelActivity) bool,
) (pct uint32, known bool, state accelActivityState) {
	if readErr != nil {
		// The baseline is kept: if the same writer's file comes back, a delta
		// over the gap is still a delta of that writer's own counters.
		if errors.Is(readErr, fs.ErrNotExist) {
			return 0, false, activityAbsent
		}
		return 0, false, activityInvalid
	}

	if t.hasBase && !t.base.sameWriter(sample) {
		t.hasBase, t.hasPct = false, false
	}

	age := now.Sub(sample.updatedAt())
	if age > accelActivityFreshWindow || age < -accelActivityFreshWindow {
		if writerAlive(sample) {
			return 0, false, activityWriterSilent
		}
		return 0, true, activityWriterGone
	}

	switch {
	case !t.hasBase:
		t.base, t.hasBase, t.hasPct = sample, true, false
	case sample.UpdatedMS == t.base.UpdatedMS:
		// No new data since the last tick: hold the last figure.
	case sample.UpdatedMS < t.base.UpdatedMS || sample.BusyMS < t.base.BusyMS:
		// The writer's counters went backwards without its identity
		// changing. Nothing sane can be derived from the pair; start over.
		t.base, t.hasPct = sample, false
	default:
		t.pct = accelActivityPercent(t.base, sample)
		t.base, t.hasPct = sample, true
	}
	if !t.hasPct {
		return 0, false, activityPending
	}
	return t.pct, true, activityFresh
}

// accelActivityPercent is the share of the interval between two samples of
// one writer that the device had a call in flight, measured entirely on the
// writer's own clock: (busy2 - busy1) / (updated2 - updated1), as a whole
// percent clamped to 0..100. The caller guarantees updated2 > updated1.
func accelActivityPercent(prev, cur accelActivity) uint32 {
	elapsed := cur.UpdatedMS - prev.UpdatedMS
	if elapsed <= 0 {
		return 0
	}
	v := (cur.BusyMS - prev.BusyMS) / elapsed * 100
	switch {
	case math.IsNaN(v) || v < 0:
		v = 0
	case v > 100:
		v = 100
	}
	return uint32(math.Round(v))
}
