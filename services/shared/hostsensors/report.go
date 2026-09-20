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
	"math"
	"time"
)

// PipeName is the named pipe nvpair-sensors listens on. Local only: the
// helper refuses remote clients at the pipe level.
const PipeName = `\\.\pipe\nvpair-sensors`

// Schema is the Report layout this package encodes and understands. A reader
// refuses a newer schema rather than guessing at its fields.
//
// It counts BREAKING changes only: a field that disappears, changes type, or
// changes meaning. Adding a new optional section — as the board sensors did —
// or a new optional field inside an existing one — as the CPU package wattage
// did — leaves it at its current value on purpose. The helper and its readers are
// deployed separately (the helper is an elevated service, the reader runs
// under the desktop app), so a helper that bumped Schema for an additive
// section would make every already-installed reader refuse the whole report
// and lose the CPU temperature it was still perfectly able to decode. New
// sections are therefore pointer fields: an older reader ignores what it does
// not know, a newer reader checks the pointer for nil.
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
	// Board is the motherboard sensor set read from the host's Super I/O
	// chip, nil on a board whose chip the helper does not support and on
	// every host where the read failed. Error describes the CPU sensor only;
	// a nil Board is reported as an absence, not as a fault.
	Board *BoardReading `json:"board,omitempty"`
	Error string        `json:"error,omitempty"`
}

// CPUReading is the CPU package temperature, the package power draw when the
// processor's energy counter is readable, and where they came from.
type CPUReading struct {
	// PackageCelsius is the package temperature in whole degrees.
	PackageCelsius uint32 `json:"package_celsius"`
	// PackageWatts is the average package power in whole watts over the
	// interval between the last two samples, derived from the processor's
	// energy counter (MSR_PKG_ENERGY_STATUS, scaled by MSR_RAPL_POWER_UNIT).
	//
	// Zero — and so absent — until the helper has two samples to subtract,
	// and permanently so on a CPU whose energy unit it could not resolve. It
	// is a separate field rather than part of the temperature because the two
	// fail apart: the energy MSRs can be missing on a part whose thermal
	// registers read perfectly, and losing the temperature over that would
	// trade a reading every consumer depends on for one that is new.
	PackageWatts float64 `json:"package_watts,omitempty"`
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

// CPUPackageWatts returns the package power draw when the report carries one
// that is non-zero and no older than maxAge as of now.
//
// It applies the same freshness rule as CPUPackage but is asked separately,
// because the two readings are independently available: a helper on a CPU
// whose energy unit it could not resolve publishes a temperature and no
// wattage, and the reverse can happen for one tick after a start, while the
// power derivative still has only one sample.
func (r Report) CPUPackageWatts(now time.Time, maxAge time.Duration) (watts float64, ok bool) {
	if r.CPU == nil || r.CPU.PackageWatts <= 0 {
		return 0, false
	}
	if r.CPU.SampledAt.IsZero() || now.Sub(r.CPU.SampledAt) > maxAge || r.CPU.SampledAt.After(now.Add(maxAge)) {
		return 0, false
	}
	return r.CPU.PackageWatts, true
}

// BoardReading is one snapshot of the motherboard sensors, read from the
// Super I/O chip over the LPC bus.
//
// What it is NOT: a readout of the board's own management controllers. ASUS's
// Dual Intelligent Processors 5 pair — the TPU (TurboV Processing Unit) and
// the EPU (Energy Processing Unit) — publish no status, load or telemetry
// interface on Windows; the board's root\wmi classes expose SMBus transfers
// and EC event control only. What those two controllers actually act on is
// this sensor set, so this is the honest readout to carry beside their
// presence.
type BoardReading struct {
	// Chip is the Super I/O part the readings came from, e.g.
	// "Nuvoton NCT6798D".
	Chip string `json:"chip"`
	// ChipID is the raw identity the chip reported, "0x<id>/0x<revision>",
	// so a field report names the part without a second tool.
	ChipID string `json:"chip_id,omitempty"`
	// Vendor and Product are the SMBIOS baseboard strings, so a reader can
	// tell whose board these sensors belong to without its own SMBIOS read.
	Vendor  string `json:"vendor,omitempty"`
	Product string `json:"product,omitempty"`
	// Temperatures carries every input the chip exposed a usable reading
	// for, in the chip's own input order. An input that is not wired up
	// reads out of range and is omitted rather than reported as zero.
	Temperatures []BoardTemp `json:"temperatures,omitempty"`
	// Fans carries the tachometer channels that reported a count. A header
	// with nothing plugged into it reports 0 RPM; a channel whose count was
	// unusable is omitted.
	Fans []BoardFan `json:"fans,omitempty"`
	// VcoreVolts is the CPU core voltage the chip's first analogue input
	// monitors, zero when it read zero (i.e. is not wired).
	VcoreVolts float64 `json:"vcore_volts,omitempty"`
	// Source names the mechanism, e.g. "superio-lpc".
	Source string `json:"source"`
	// SampledAt is when the helper took this reading; readers drop a reading
	// older than their freshness window instead of repeating a frozen number.
	SampledAt time.Time `json:"sampled_at"`
}

// Board temperature roles. Role classifies an input by what it measures, so a
// reader can pick one without knowing the chip's input names. Inputs whose
// meaning is board-specific keep RoleAux: guessing which of a chip's spare
// inputs an individual board wired to its VRM would be a fabrication, and a
// wrong guess reads back as a real temperature.
//
// RoleVRM is part of the contract but no producer claims it yet, because
// nothing in a Super I/O chip reports whether a probe header actually has a
// probe in it — see the note on TSENSOR in nvpair-sensors/superio.go for the
// measurement behind that. It stays so a producer that can establish it (a
// board table, a vendor interface) has somewhere to put it, and so readers
// need no change when one appears.
const (
	RoleCPUSocket   = "cpu-socket"  // CPUTIN, the socket-side thermistor
	RoleMotherboard = "motherboard" // SYSTIN, the board's own sensor
	RoleVRM         = "vrm"         // a verified VRM / probe-header reading
	RolePCH         = "pch"         // the chipset's own sensors
	RolePECI        = "peci"        // the CPU's own PECI readout, via the chip
	RoleDIMM        = "dimm"        // per-channel memory sensors
	RoleSMBus       = "smbus"       // sensors the chip reaches over SMBus
	RoleAux         = "aux"         // an input whose meaning is board-specific
	RoleVirtual     = "virtual"     // a computed input, not a physical sensor
)

// BoardTemp is one temperature input.
type BoardTemp struct {
	// Label is the human form, e.g. "CPU socket".
	Label string `json:"label"`
	// Input is the chip's own name for it, e.g. "CPUTIN" — the name a
	// datasheet or LibreHardwareMonitor uses.
	Input string `json:"input"`
	// Role classifies the input; see the Role constants.
	Role string `json:"role,omitempty"`
	// Celsius is the reading. The chip resolves half a degree, so this is
	// not a whole number.
	Celsius float64 `json:"celsius"`
}

// BoardFan is one tachometer channel.
type BoardFan struct {
	// Index is the chip's channel number, 0-based.
	Index int `json:"index"`
	// Label is the human form, e.g. "Fan 1".
	Label string `json:"label"`
	// RPM is the measured speed; 0 means the header is idle or empty.
	RPM uint32 `json:"rpm"`
}

// BoardRowCelsius returns the one temperature that represents the board, for
// a reader that shows a single figure: the VRM / T_Sensor probe when the
// board wired one up, else the board's own sensor. ok is false for a report
// without a board section, one whose inputs carry neither, and a stale one —
// the same freshness rule CPUPackage applies.
//
// The reading is rounded to whole degrees, which is all a node inventory
// carries.
func (r Report) BoardRowCelsius(now time.Time, maxAge time.Duration) (celsius uint32, ok bool) {
	if r.Board == nil {
		return 0, false
	}
	if r.Board.SampledAt.IsZero() || now.Sub(r.Board.SampledAt) > maxAge || r.Board.SampledAt.After(now.Add(maxAge)) {
		return 0, false
	}
	for _, role := range []string{RoleVRM, RoleMotherboard} {
		for _, t := range r.Board.Temperatures {
			if t.Role != role || t.Celsius <= 0 {
				continue
			}
			return uint32(math.Round(t.Celsius)), true
		}
	}
	return 0, false
}
