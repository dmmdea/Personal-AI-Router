// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import "testing"

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
