// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"
	"time"
)

// The register values below were read live through PawnIO on 2026-09-19:
// an i9-10980XE (TjMax 110) and an i7-11800H (TjMax 100).

func TestDecodeTjMax(t *testing.T) {
	cases := []struct {
		name string
		raw  uint64
		want uint32
		ok   bool
	}{
		{"i9-10980XE", 0x00000000006E0A00, 110, true},
		{"i7-11800H", 0x000000008A640000, 100, true},
		{"unreported", 0x0000000000000A00, 0, false},
		{"only bits 23:16 count", 0xFFFFFFFFFF64FFFF, 100, true},
	}
	for _, c := range cases {
		got, ok := decodeTjMax(c.raw)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: decodeTjMax(0x%X) = (%d, %v), want (%d, %v)", c.name, c.raw, got, ok, c.want, c.ok)
		}
	}
}

func TestDecodeThermStatus(t *testing.T) {
	cases := []struct {
		name  string
		raw   uint64
		delta uint32
		valid bool
	}{
		{"i9-10980XE package", 0x0000000088340800, 52, true},
		{"i9-10980XE core", 0x0000000088360800, 54, true},
		{"i7-11800H package", 0x0000000088380802, 56, true},
		{"i7-11800H core", 0x00000000883B8802, 59, true},
		{"reading not valid", 0x0000000008340800, 52, false},
		{"bit 23 is not part of the readout", 0x0000000080B40800, 52, true},
	}
	for _, c := range cases {
		delta, valid := decodeThermStatus(c.raw)
		if delta != c.delta || valid != c.valid {
			t.Errorf("%s: decodeThermStatus(0x%X) = (%d, %v), want (%d, %v)", c.name, c.raw, delta, valid, c.delta, c.valid)
		}
	}
}

func TestPackageCelsius(t *testing.T) {
	if c, ok := packageCelsius(110, 52); !ok || c != 58 {
		t.Fatalf("packageCelsius(110, 52) = (%d, %v), want (58, true)", c, ok)
	}
	if c, ok := packageCelsius(100, 56); !ok || c != 44 {
		t.Fatalf("packageCelsius(100, 56) = (%d, %v), want (44, true)", c, ok)
	}
	if c, ok := packageCelsius(100, 100); !ok || c != 0 {
		t.Fatalf("packageCelsius(100, 100) = (%d, %v), want (0, true)", c, ok)
	}
	if _, ok := packageCelsius(100, 101); ok {
		t.Fatal("a readout past TjMax must be rejected")
	}
}

// TestDecodeEnergyUnit pins the RAPL energy scale. ESU=14 (61.0 uJ per tick)
// is what desktop and mobile Core parts report; ESU=16 (15.3 uJ) appears on
// several server and Atom parts. A zero is not a scale any part uses, so it
// means the register read was not what we think it was.
func TestDecodeEnergyUnit(t *testing.T) {
	// Bits 12:8 = 14, with plausible power (3:0) and time (19:16) units set,
	// so the extraction is proven to ignore its neighbours.
	joules, ok := decodeEnergyUnit(0x000A0E03)
	if !ok {
		t.Fatal("ESU 14 reported as unavailable")
	}
	if want := 1.0 / 16384; joules != want {
		t.Fatalf("joulesPerTick = %v, want %v", joules, want)
	}

	if joules, ok := decodeEnergyUnit(0x00141003); !ok || joules != 1.0/65536 {
		t.Fatalf("ESU 16: %v, %v", joules, ok)
	}
	if _, ok := decodeEnergyUnit(0x000A0003); ok {
		t.Fatal("ESU 0 must be reported as unavailable, not as one joule per tick")
	}
	if _, ok := decodeEnergyUnit(0); ok {
		t.Fatal("an all-zero register must be reported as unavailable")
	}
}

// TestPkgEnergyCounterIgnoresReservedBits: bits 63:32 are reserved and folding
// them in would turn every reading into a wildly wrong delta.
func TestPkgEnergyCounterIgnoresReservedBits(t *testing.T) {
	if got := pkgEnergyCounter(0xDEADBEEF_0000FFFF); got != 0x0000FFFF {
		t.Fatalf("pkgEnergyCounter = %#x, want 0xFFFF", got)
	}
}

// TestEnergyDeltaWrapsAtThirtyTwoBits: the counter rolls over roughly every
// hour on a busy package, and the whole reason the delta is expressed as
// unsigned subtraction is that it stays correct across that point.
func TestEnergyDeltaWrapsAtThirtyTwoBits(t *testing.T) {
	if got := energyDelta(100, 500); got != 400 {
		t.Fatalf("energyDelta = %d, want 400", got)
	}
	if got := energyDelta(0xFFFFFF00, 0x000000FF); got != 0x1FF {
		t.Fatalf("energyDelta across the wrap = %#x, want 0x1FF", got)
	}
	if got := energyDelta(7, 7); got != 0 {
		t.Fatalf("energyDelta of a stalled counter = %d, want 0", got)
	}
}

// TestPackageWatts derives power the way the sampler does, and pins every
// case where it must decline rather than publish a number.
func TestPackageWatts(t *testing.T) {
	const esu14 = 1.0 / 16384 // 61.035 uJ per tick

	// 2_293_760 ticks x 61.035 uJ = 140.0 J in 1 s = 140 W.
	if w, ok := packageWatts(2_293_760, esu14, time.Second); !ok || w != 140 {
		t.Fatalf("packageWatts = %v, %v; want 140, true", w, ok)
	}
	// Same energy over two seconds is half the power.
	if w, ok := packageWatts(2_293_760, esu14, 2*time.Second); !ok || w != 70 {
		t.Fatalf("packageWatts over 2 s = %v, %v; want 70, true", w, ok)
	}
	// Rounded to whole watts: 18.4 J in 1 s reads 18 W.
	if w, ok := packageWatts(301_465, esu14, time.Second); !ok || w != 18 {
		t.Fatalf("packageWatts = %v, %v; want 18, true", w, ok)
	}

	for _, c := range []struct {
		name    string
		ticks   uint32
		joules  float64
		elapsed time.Duration
	}{
		{name: "no energy unit", ticks: 1000, joules: 0, elapsed: time.Second},
		{name: "zero interval", ticks: 1000, joules: esu14, elapsed: 0},
		{name: "backwards clock", ticks: 1000, joules: esu14, elapsed: -time.Second},
		{name: "counter did not move", ticks: 0, joules: esu14, elapsed: time.Second},
		{name: "a re-based counter", ticks: 0xFFFFFF00, joules: esu14, elapsed: time.Second},
		{name: "sub-watt rounds to nothing", ticks: 1, joules: esu14, elapsed: time.Second},
	} {
		if w, ok := packageWatts(c.ticks, c.joules, c.elapsed); ok {
			t.Errorf("%s: packageWatts = %v, true; want no figure", c.name, w)
		}
	}
}
