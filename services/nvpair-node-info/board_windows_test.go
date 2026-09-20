// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"nvpair-shared/hostsensors"
	"nvpair-shared/noderec"
)

func boardReport(at time.Time, vendor, product string, temps ...hostsensors.BoardTemp) hostsensors.Report {
	return hostsensors.Report{
		HelperVersion: "0.2.0",
		CPU:           &hostsensors.CPUReading{PackageCelsius: 58, Source: "intel-msr", SampledAt: at},
		Board: &hostsensors.BoardReading{
			Chip:         "Nuvoton NCT6798D",
			ChipID:       "0xD4/0x2B",
			Vendor:       vendor,
			Product:      product,
			Temperatures: temps,
			Source:       "superio-lpc",
			SampledAt:    at,
		},
	}
}

func motherboardAt(c float64) hostsensors.BoardTemp {
	return hostsensors.BoardTemp{Label: "Motherboard", Input: "SYSTIN", Role: hostsensors.RoleMotherboard, Celsius: c}
}

func vrmAt(c float64) hostsensors.BoardTemp {
	return hostsensors.BoardTemp{Label: "T_Sensor", Input: "TSENSOR", Role: hostsensors.RoleVRM, Celsius: c}
}

// TestBoardRowName pins the naming rule: a board sold under the ROG line gets
// the ROG name, any other ASUS board the plain one, and a board this does not
// know whose it is gets no row at all rather than a generic one.
func TestBoardRowName(t *testing.T) {
	cases := []struct {
		vendor, product string
		want            string
		wantOK          bool
	}{
		{"ASUSTeK COMPUTER INC.", "ROG STRIX X299-E GAMING II", boardNameROG, true},
		{"ASUSTeK Computer Inc.", "ROG MAXIMUS XII FORMULA", boardNameROG, true},
		{"ASUSTeK COMPUTER INC.", "PRIME Z390-A", boardNameASUS, true},
		{"ASUSTeK COMPUTER INC.", "", boardNameASUS, true},
		{"Micro-Star International Co., Ltd.", "MAG B550 TOMAHAWK", "", false},
		{"Gigabyte Technology Co., Ltd.", "X570 AORUS ELITE", "", false},
		{"", "ROG STRIX X299-E GAMING II", "", false},
	}
	for _, tc := range cases {
		got, ok := boardRowName(tc.vendor, tc.product)
		if ok != tc.wantOK || got != tc.want {
			t.Errorf("boardRowName(%q, %q) = (%q, %v), want (%q, %v)",
				tc.vendor, tc.product, got, ok, tc.want, tc.wantOK)
		}
	}
}

// TestBoardPollerPublishesOneRowWithATemperature: the row is the board's
// controllers, its kind marks it as not-a-GPU, and it carries a temperature
// and nothing else — no utilization, because neither controller reports one.
func TestBoardPollerPublishesOneRowWithATemperature(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	p := newBoardPoller(func() time.Time { return now })

	if _, ok := p.Row(); ok {
		t.Fatal("a row was published before any report arrived")
	}
	p.observe(boardReport(now, "ASUSTeK COMPUTER INC.", "ROG STRIX X299-E GAMING II", motherboardAt(33.5)), nil)

	row, ok := p.Row()
	if !ok {
		t.Fatal("no row after a report describing an ASUS board")
	}
	if row.Name != boardNameROG {
		t.Fatalf("row name = %q, want %q", row.Name, boardNameROG)
	}
	if row.Kind != noderec.GPUKindBoard {
		t.Fatalf("row kind = %q, want %q", row.Kind, noderec.GPUKindBoard)
	}
	if row.statsKey != boardStatsKey {
		t.Fatalf("row statsKey = %q, want %q", row.statsKey, boardStatsKey)
	}
	if row.UtilizationPercent != 0 || row.VramBytes != 0 {
		t.Fatalf("row carries GPU fields it cannot know: %+v", row)
	}
	st, ok := p.Latest()
	if !ok || st.TemperatureC != 34 {
		t.Fatalf("temperature = %+v (ok=%v), want 34 °C from the rounded 33.5", st, ok)
	}
}

// TestBoardPollerPrefersTheVRMProbe: when the board wired its T_Sensor / VRM
// input, that is the figure the row shows; the board's own sensor is the
// fallback, not the first choice.
func TestBoardPollerPrefersTheVRMProbe(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	p := newBoardPoller(func() time.Time { return now })
	p.observe(boardReport(now, "ASUSTeK COMPUTER INC.", "ROG STRIX X299-E GAMING II",
		motherboardAt(33), vrmAt(51)), nil)

	st, ok := p.Latest()
	if !ok || st.TemperatureC != 51 {
		t.Fatalf("temperature = %+v (ok=%v), want the VRM probe's 51 °C", st, ok)
	}
}

// TestBoardPollerKeepsTheRowWhenTheHelperGoesAway: a motherboard does not
// leave. An unreachable helper costs the row its temperature — the Temp line
// disappears rather than freezing — but never its place in the inventory.
func TestBoardPollerKeepsTheRowWhenTheHelperGoesAway(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	p := newBoardPoller(func() time.Time { return now })
	p.observe(boardReport(now, "ASUSTeK COMPUTER INC.", "ROG STRIX X299-E GAMING II", motherboardAt(33)), nil)

	p.observe(hostsensors.Report{}, errors.New("the pipe does not exist"))
	if _, ok := p.Row(); !ok {
		t.Fatal("an unreachable helper dropped the board row")
	}
	if st, ok := p.Latest(); ok {
		t.Fatalf("temperature = %+v, want none while the helper is unreachable", st)
	}

	// And it comes back when the helper does.
	p.observe(boardReport(now, "ASUSTeK COMPUTER INC.", "ROG STRIX X299-E GAMING II", motherboardAt(36)), nil)
	if st, ok := p.Latest(); !ok || st.TemperatureC != 36 {
		t.Fatalf("temperature = %+v (ok=%v) after the helper returned", st, ok)
	}
}

// TestBoardPollerPublishesNothingItCannotEstablish: a helper with no board
// section (an unsupported Super I/O chip), and a board that is not ASUS's,
// both produce no row — presence is claimed from evidence, not assumed.
func TestBoardPollerPublishesNothingItCannotEstablish(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)

	t.Run("helper without a board section", func(t *testing.T) {
		p := newBoardPoller(func() time.Time { return now })
		p.observe(hostsensors.Report{
			HelperVersion: "0.2.0",
			CPU:           &hostsensors.CPUReading{PackageCelsius: 58, SampledAt: now},
		}, nil)
		if _, ok := p.Row(); ok {
			t.Fatal("a row appeared without a board section to describe it")
		}
	})

	t.Run("another vendor's board", func(t *testing.T) {
		p := newBoardPoller(func() time.Time { return now })
		p.observe(boardReport(now, "Micro-Star International Co., Ltd.", "MAG B550 TOMAHAWK", motherboardAt(33)), nil)
		if _, ok := p.Row(); ok {
			t.Fatal("a non-ASUS board got an ASUS controller row")
		}
		if _, ok := p.Latest(); ok {
			t.Fatal("a non-ASUS board published a temperature under the board key")
		}
	})
}

// TestBoardPollerDropsAStaleReading: a helper that stopped sampling must not
// have its last number repeated forever, the same rule the CPU package
// reading follows.
func TestBoardPollerDropsAStaleReading(t *testing.T) {
	sampled := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	now := sampled.Add(boardTempMaxAge + time.Second)
	p := newBoardPoller(func() time.Time { return now })
	p.observe(boardReport(sampled, "ASUSTeK COMPUTER INC.", "ROG STRIX X299-E GAMING II", motherboardAt(33)), nil)

	if _, ok := p.Row(); !ok {
		t.Fatal("a stale reading dropped the row; only the temperature is time-limited")
	}
	if st, ok := p.Latest(); ok {
		t.Fatalf("temperature = %+v, want none for a reading older than the window", st)
	}
}

// TestBoardRowReachesTheResponseBody is the join: the row the poller
// published and the temperature it sampled have to meet in the JSON under one
// statsKey, with no utilization_percent and no vram field on that row.
func TestBoardRowReachesTheResponseBody(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	p := newBoardPoller(func() time.Time { return now })
	p.observe(boardReport(now, "ASUSTeK COMPUTER INC.", "ROG STRIX X299-E GAMING II", vrmAt(47)), nil)

	row, _ := p.Row()
	st, _ := p.Latest()
	snap := statsSnapshot{
		GPUInventory: []GPUInfo{row},
		GPU:          map[string]gpuStat{boardStatsKey: st},
	}
	body := buildResponseAt(nil, nil, 0, snap, "host", nil, now)

	var got struct {
		GPUs []map[string]any `json:"gpus"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.GPUs) != 1 {
		t.Fatalf("gpus = %v, want exactly the board row", got.GPUs)
	}
	board := got.GPUs[0]
	if board["name"] != boardNameROG || board["kind"] != "board" {
		t.Fatalf("board row = %v", board)
	}
	if board["temperature_celsius"] != float64(47) {
		t.Fatalf("temperature_celsius = %v, want 47", board["temperature_celsius"])
	}
	for _, absent := range []string{"utilization_percent", "vram_bytes", "vram_used_bytes"} {
		if _, present := board[absent]; present {
			t.Fatalf("board row carries %s: %v", absent, board)
		}
	}
	if strings.Contains(string(body), boardStatsKey) {
		t.Fatalf("the join key leaked onto the wire: %s", body)
	}
}

// TestMergeBoardStatsLeavesThePreviousMapAlone: the snapshot's GPU map can be
// the previous tick's already-published one (applyGPUStats aliases it when a
// collection fails). Writing the board temperature into it in place would
// mutate a map other goroutines are reading.
func TestMergeBoardStatsLeavesThePreviousMapAlone(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	c := newStatsCollector(func() []GPUInfo { return nil })
	c.board = newBoardPoller(func() time.Time { return now })
	c.board.observe(boardReport(now, "ASUSTeK COMPUTER INC.", "ROG STRIX X299-E GAMING II", motherboardAt(33)), nil)

	published := map[string]gpuStat{"luid_0x00000000_0x000054f0_phys_0": {UtilizationPct: 12}}
	snap := &statsSnapshot{GPU: published}
	c.mergeBoardStats(snap)

	if _, mutated := published[boardStatsKey]; mutated {
		t.Fatal("mergeBoardStats wrote into the already-published map")
	}
	if st, ok := snap.GPU[boardStatsKey]; !ok || st.TemperatureC != 33 {
		t.Fatalf("snapshot board entry = %+v (ok=%v)", st, ok)
	}
	if snap.GPU["luid_0x00000000_0x000054f0_phys_0"].UtilizationPct != 12 {
		t.Fatal("mergeBoardStats lost the GPU entries it copied")
	}
	if !snap.GPUSampledAt.IsZero() {
		t.Fatal("a board sample marked GPU telemetry fresh")
	}
}

// TestSnapshotAppendsTheBoardRow: the row reaches the response through the
// inventory the same way a late-detected adapter does, and appending it must
// not mutate the stored inventory slice.
func TestSnapshotAppendsTheBoardRow(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	c := newStatsCollector(func() []GPUInfo { return nil })
	c.board = newBoardPoller(func() time.Time { return now })

	detected := []GPUInfo{{Name: "NVIDIA A2", statsKey: "luid_a"}}
	c.gpuInventory.Store(&detected)

	if got := c.Snapshot().GPUInventory; len(got) != 1 {
		t.Fatalf("inventory = %v before any board report, want just the GPU", got)
	}
	c.board.observe(boardReport(now, "ASUSTeK COMPUTER INC.", "ROG STRIX X299-E GAMING II", motherboardAt(33)), nil)

	got := c.Snapshot().GPUInventory
	if len(got) != 2 || got[1].Kind != noderec.GPUKindBoard {
		t.Fatalf("inventory = %+v, want the GPU plus one board row", got)
	}
	if len(detected) != 1 {
		t.Fatalf("Snapshot appended into the stored inventory slice: %+v", detected)
	}
	// Two snapshots must not accumulate rows.
	if got = c.Snapshot().GPUInventory; len(got) != 2 {
		t.Fatalf("a second snapshot produced %d rows: %+v", len(got), got)
	}
}
