// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"nvpair-shared/hostsensors"
	"nvpair-shared/noderec"
)

// The motherboard controller row.
//
// ASUS fits its boards with two small microcontrollers it markets together as
// Dual Intelligent Processors 5: the TPU (TurboV Processing Unit), which
// drives clocks and voltages, and the EPU (Energy Processing Unit), which
// manages power. They are as much a part of what this node is made of as an
// accelerator card, so the inventory lists them — and that is where the
// honesty problem starts, because they publish nothing.
//
// Measured on the target board (ASUS ROG STRIX X299-E GAMING II, BIOS 2103):
// neither controller exposes a status, load or telemetry interface on
// Windows. The board's root\wmi namespace carries ASUSHW (raw SMBus reads and
// writes), ASUSManagement (display and EC event control) and AsusWpbtWmi; the
// sensor_get_* methods the Ryzen-era ASUS boards answer do not exist on X299,
// and installing Armoury Crate does not add them. So there is no utilization
// to publish, and publishing a zero would render as an idle device rather
// than as an unknown one.
//
// What the row therefore carries is what can be established: presence, from
// the SMBIOS baseboard strings, and one temperature from the board sensors
// those two controllers actually act on — the Super I/O chip's inputs, read
// by the elevated nvpair-sensors helper (see its README) and delivered over
// the same named pipe the CPU package temperature already travels on.

const (
	// boardStatsKey joins the static row to its temperature in the stats
	// snapshot. Namespaced so it can never collide with the PDH LUID keys
	// the GPU rows use or the "hailo:" keys the accelerators use.
	boardStatsKey = "board:superio"

	// boardTempMaxAge is the freshness window for a board reading. It
	// matches the CPU package reading's window, because both come from the
	// same report: a helper that stopped sampling must drop both, not keep
	// repeating one frozen number.
	boardTempMaxAge = 30 * time.Second

	// asusVendorMarker is what ASUS writes in SMBIOS. Matched as a
	// substring, upper-cased, because the field has carried "ASUSTeK
	// COMPUTER INC." and "ASUSTeK Computer Inc." across BIOS generations.
	asusVendorMarker = "ASUSTEK"

	// rogMarker picks the ROG name for a board sold under that line.
	rogMarker = "ROG"

	boardNameROG  = "ROG Dual Intelligent Processors"
	boardNameASUS = "ASUS Dual Intelligent Processors"
)

// boardRowName is the inventory name for a board. ok is false for a board
// this does not claim: a non-ASUS one, or one whose SMBIOS manufacturer is
// blank — listing a board's controllers by name means knowing whose board it
// is.
func boardRowName(vendor, product string) (string, bool) {
	if !strings.Contains(strings.ToUpper(vendor), asusVendorMarker) {
		return "", false
	}
	if strings.Contains(strings.ToUpper(product), rogMarker) {
		return boardNameROG, true
	}
	return boardNameASUS, true
}

// boardPoller holds the board row once the helper has described it, and the
// latest temperature to hang on it.
//
// It does not poll on its own: it is fed the reports the CPU temperature
// poller already fetches from the helper's pipe, on that poller's 5 s
// cadence. One report carries both readings, so a second connection would
// buy nothing but a second connection.
type boardPoller struct {
	now func() time.Time

	// row is published once a report describes a board this claims. It is
	// kept from then on: a motherboard does not leave, so an unreachable
	// helper costs the row its temperature, never its existence.
	row atomic.Pointer[GPUInfo]

	// latest is the temperature in whole degrees, zero when the newest
	// report carried none or was stale.
	latest atomic.Uint32

	// announced belongs to the feeding goroutine; it keeps the description
	// of what was found to one log line.
	announced bool
}

func newBoardPoller(now func() time.Time) *boardPoller {
	return &boardPoller{now: now}
}

// observe takes one report from the helper. Called from the CPU temperature
// poller's goroutine, which is the only writer.
func (p *boardPoller) observe(r hostsensors.Report, err error) {
	if p == nil {
		return
	}
	if err != nil || r.Board == nil {
		// An unreachable helper, or one whose board section is absent
		// because the chip is not one it reads. Either way there is no
		// fresh temperature; a row already published keeps its place.
		p.latest.Store(0)
		return
	}
	name, ok := boardRowName(r.Board.Vendor, r.Board.Product)
	if !ok {
		p.latest.Store(0)
		return
	}
	if !p.announced {
		slog.Info("motherboard controller detected",
			"row", name,
			"board", strings.TrimSpace(r.Board.Vendor+" "+r.Board.Product),
			"sensor_chip", r.Board.Chip,
			"sensor_chip_id", r.Board.ChipID,
			"controllers", "ASUS TPU (TurboV Processing Unit) + EPU (Energy Processing Unit), Dual Intelligent Processors 5",
			"utilization", "not published: neither controller exposes a status or load interface on Windows")
		p.announced = true
	}
	if p.row.Load() == nil {
		p.row.Store(&GPUInfo{
			Name:     name,
			Kind:     noderec.GPUKindBoard,
			statsKey: boardStatsKey,
		})
	}
	celsius, ok := r.BoardRowCelsius(p.now(), boardTempMaxAge)
	if !ok {
		p.latest.Store(0)
		return
	}
	p.latest.Store(celsius)
}

// Row returns the published inventory row, or false before the helper has
// described a board this claims.
func (p *boardPoller) Row() (GPUInfo, bool) {
	if p == nil {
		return GPUInfo{}, false
	}
	row := p.row.Load()
	if row == nil {
		return GPUInfo{}, false
	}
	return *row, true
}

// Latest returns the board's temperature sample. ok is false while there is
// none, which drops temperature_celsius from the row rather than publishing a
// zero.
func (p *boardPoller) Latest() (gpuStat, bool) {
	if p == nil {
		return gpuStat{}, false
	}
	c := p.latest.Load()
	if c == 0 {
		return gpuStat{}, false
	}
	return gpuStat{TemperatureC: c}, true
}
