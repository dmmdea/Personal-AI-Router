// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package hostsensors is the wire contract between nvpair-sensors — the
// elevated Windows host-sensor helper — and the services that read it
// (nvpair-node-info).
//
// Windows has no unprivileged CPU temperature source: the package sensor is a
// model-specific register that only ring 0 can read, and the signed PawnIO
// driver that exposes it admits administrators only. nvpair-node-info runs
// unprivileged under the desktop app, so the read happens in a small
// LocalSystem service (nvpair-sensors) and travels over a named pipe. Every
// connection to the pipe receives one JSON Report and is closed; there is no
// request body, no long-lived stream and no port.
package hostsensors

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// PipeName is the named pipe nvpair-sensors listens on. Local only: the
// helper refuses remote clients at the pipe level.
const PipeName = `\\.\pipe\nvpair-sensors`

// Schema is the Report layout this package encodes and understands. A reader
// refuses a newer schema rather than guessing at its fields.
const Schema = 1

// MaxReportBytes bounds one Report on the wire; a reader stops there.
const MaxReportBytes = 64 << 10

// Report is one snapshot of the host sensors the helper can read.
//
// CPU is nil — and Error says why — when the helper is running but has no
// sensor to read: PawnIO is not installed, the process is not elevated, or
// the CPU is not one the embedded module supports. Readers treat a nil CPU
// exactly like an unreachable helper: the temperature is omitted, never
// reported as zero.
type Report struct {
	Schema        int         `json:"schema"`
	HelperVersion string      `json:"helper_version,omitempty"`
	CPU           *CPUReading `json:"cpu,omitempty"`
	Error         string      `json:"error,omitempty"`
}

// CPUReading is the CPU package temperature and where it came from.
type CPUReading struct {
	// PackageCelsius is the package temperature in whole degrees.
	PackageCelsius uint32 `json:"package_celsius"`
	// TjMaxCelsius is the junction maximum the readout is relative to
	// (IA32_TEMPERATURE_TARGET on Intel), for readers that want the margin.
	TjMaxCelsius uint32 `json:"tjmax_celsius,omitempty"`
	// Source names the mechanism, e.g. "intel-msr".
	Source string `json:"source"`
	// SampledAt is when the helper took this reading; readers drop a reading
	// older than their freshness window instead of repeating a frozen number.
	SampledAt time.Time `json:"sampled_at"`
}

// ErrSchema is returned by Decode for a Report newer than this package.
var ErrSchema = errors.New("hostsensors: report schema newer than this reader")

// Encode writes r as one JSON document terminated by a newline.
func Encode(w io.Writer, r Report) error {
	r.Schema = Schema
	enc := json.NewEncoder(w)
	return enc.Encode(r)
}

// Decode reads one Report. It bounds the read at MaxReportBytes and refuses a
// schema newer than Schema.
func Decode(r io.Reader) (Report, error) {
	var out Report
	dec := json.NewDecoder(io.LimitReader(r, MaxReportBytes))
	if err := dec.Decode(&out); err != nil {
		return Report{}, fmt.Errorf("hostsensors: decode report: %w", err)
	}
	if out.Schema > Schema {
		return Report{}, fmt.Errorf("%w: got %d, support %d", ErrSchema, out.Schema, Schema)
	}
	return out, nil
}

// CPUPackage returns the package temperature when the report carries one
// that is non-zero and no older than maxAge as of now. ok is false for a
// helper without a sensor (CPU nil), a zero reading and a stale one alike.
func (r Report) CPUPackage(now time.Time, maxAge time.Duration) (celsius uint32, ok bool) {
	if r.CPU == nil || r.CPU.PackageCelsius == 0 {
		return 0, false
	}
	if r.CPU.SampledAt.IsZero() || now.Sub(r.CPU.SampledAt) > maxAge || r.CPU.SampledAt.After(now.Add(maxAge)) {
		return 0, false
	}
	return r.CPU.PackageCelsius, true
}
