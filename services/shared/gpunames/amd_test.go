// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package gpunames

import (
	"strings"
	"testing"
)

// TestAMD walks the table across every architecture it covers, including the
// three ids that are easy to transpose: 0x1900 is Hawk Point and not Strix
// Point, 0x150e is Strix Point and not Strix Halo, 0x1586 is Strix Halo.
func TestAMD(t *testing.T) {
	cases := []struct {
		id   uint32
		want string
	}{
		{0x15dd, "AMD Radeon Vega Graphics (Raven Ridge, GCN 5)"},
		{0x15d8, "AMD Radeon Vega Graphics (Picasso, GCN 5)"},
		{0x1636, "AMD Radeon Vega Graphics (Renoir, GCN 5.1)"},
		{0x164c, "AMD Radeon Vega Graphics (Lucienne, GCN 5.1)"},
		{0x1638, "AMD Radeon Vega Graphics (Cezanne, GCN 5.1)"},
		{0x15e7, "AMD Radeon Vega Graphics (Barcelo, GCN 5.1)"},
		{0x164e, "AMD Radeon Graphics (Raphael, RDNA 2)"},
		{0x1681, "AMD Radeon 680M (Rembrandt, RDNA 2)"},
		{0x15bf, "AMD Radeon 780M (Phoenix, RDNA 3)"},
		{0x15c8, "AMD Radeon 740M (Phoenix2, RDNA 3)"},
		{0x1900, "AMD Radeon 780M (Hawk Point, RDNA 3)"},
		{0x150e, "AMD Radeon 890M (Strix Point, RDNA 3.5)"},
		{0x1586, "AMD Radeon 8060S (Strix Halo, RDNA 3.5)"},
		{0x1114, "AMD Radeon 860M (Krackan Point, RDNA 3.5)"},
	}
	for _, c := range cases {
		got, ok := AMD(c.id)
		if !ok {
			t.Errorf("AMD(%#04x) reported unknown, want %q", c.id, c.want)
			continue
		}
		if got != c.want {
			t.Errorf("AMD(%#04x) = %q, want %q", c.id, got, c.want)
		}
	}
}

// TestAMDUnknownID: the table is APU-only, so a discrete Radeon (0x73bf is
// Navi 21) must report unknown and leave the caller's own name alone.
func TestAMDUnknownID(t *testing.T) {
	for _, id := range []uint32{0x0000, 0x7480, 0x73bf, 0x1234} {
		if name, ok := AMD(id); ok {
			t.Errorf("AMD(%#04x) = %q, want unknown", id, name)
		}
	}
}

// TestAMDName covers the fallback ladder the Linux inventory depends on,
// including the architecture clause it fills from the driver's IP discovery
// table for a part released after this table was written.
func TestAMDName(t *testing.T) {
	cases := []struct {
		id     uint32
		haveID bool
		arch   string
		want   string
	}{
		{0x15e7, true, "", "AMD Radeon Vega Graphics (Barcelo, GCN 5.1)"},
		{0x7480, true, "", "AMD Radeon Graphics (device 0x7480)"},
		{0x1234, true, "RDNA 3.5", "AMD Radeon Graphics (device 0x1234, RDNA 3.5)"},
		{0x0b69, true, "", "AMD Radeon Graphics (device 0x0b69)"},
		{0x0000, false, "", "AMD Radeon Graphics"},
		{0x0000, false, "RDNA 4", "AMD Radeon Graphics"},
		// The table is authoritative: a listed id keeps its verified name even
		// when the caller hands in an architecture that maps elsewhere.
		{0x15e7, true, "RDNA 4", "AMD Radeon Vega Graphics (Barcelo, GCN 5.1)"},
	}
	for _, c := range cases {
		if got := AMDName(c.id, c.haveID, c.arch); got != c.want {
			t.Errorf("AMDName(%#04x, %v, %q) = %q, want %q", c.id, c.haveID, c.arch, got, c.want)
		}
	}
}

// TestAMDAPU pins the memory verdict: every listed part is an integrated GPU
// drawing from a unified pool, and an unlisted id must report known=false so
// the caller falls through to judging the card by its own sysfs numbers.
func TestAMDAPU(t *testing.T) {
	for id := range amdModels {
		apu, known := AMDAPU(id)
		if !known {
			t.Errorf("AMDAPU(%#04x) reported unknown for a listed id", id)
		}
		if !apu {
			t.Errorf("AMDAPU(%#04x) = false; every listed part is an integrated GPU", id)
		}
	}
	if apu, known := AMDAPU(0x73bf); apu || known {
		t.Errorf("AMDAPU(0x73bf) = (%v, %v), want (false, false) for an unlisted discrete card", apu, known)
	}
}

// TestAMDArchitecture pins the Graphics Core IP mapping, including the two
// families deliberately left out: GC 9.4.x (Arcturus / Aldebaran / MI300) and
// 9.5.x (MI350) are the data-center CDNA line, so the "9.x is GCN 5.x"
// shorthand must NOT name them.
func TestAMDArchitecture(t *testing.T) {
	cases := []struct {
		major, minor uint64
		want         string
		ok           bool
	}{
		{9, 0, "GCN 5", true},
		{9, 1, "GCN 5", true},
		{9, 2, "GCN 5", true},
		{9, 3, "GCN 5.1", true},
		{10, 1, "RDNA", true},
		{10, 3, "RDNA 2", true},
		{11, 0, "RDNA 3", true},
		{11, 5, "RDNA 3.5", true},
		{12, 0, "RDNA 4", true},
		{9, 4, "", false},
		{9, 5, "", false},
		{13, 0, "", false},
		{0, 0, "", false},
	}
	for _, c := range cases {
		got, ok := AMDArchitecture(c.major, c.minor)
		if got != c.want || ok != c.ok {
			t.Errorf("AMDArchitecture(%d, %d) = (%q, %v), want (%q, %v)",
				c.major, c.minor, got, ok, c.want, c.ok)
		}
	}
}

// TestAMDNamesTheGenerationAndNeverTheCoreCount is the rule the table exists
// to enforce, and it moved here with the table it walks. The compute-unit
// count is NOT derivable from the device id - 0x15e7 alone ships as Vega 6,
// Vega 7 and Vega 8, separated only by the PCI revision id - so printing one
// is a guess that is wrong on most SKUs. The generation is derivable, and is
// what the operator asked to see.
func TestAMDNamesTheGenerationAndNeverTheCoreCount(t *testing.T) {
	architectures := []string{"GCN 5", "GCN 5.1", "RDNA 2", "RDNA 3", "RDNA 3.5"}
	for id := range amdModels {
		name := AMDName(id, true, "")
		named := false
		for _, arch := range architectures {
			if strings.HasSuffix(name, ", "+arch+")") {
				named = true
				break
			}
		}
		if !named {
			t.Errorf("amdModels[%#04x] = %q; every name must end in a known architecture %v", id, name, architectures)
		}
		if !strings.Contains(name, " (") {
			t.Errorf("amdModels[%#04x] = %q; want the \"<name> (<codename>, <architecture>)\" shape", id, name)
		}
		// "Vega 7" and friends are core counts, not generations. "Vega
		// Graphics" (no digit) is the marketing name AMD itself uses for the
		// whole family and is allowed.
		for _, banned := range []string{"Vega 3", "Vega 6", "Vega 7", "Vega 8", "Vega 10", "Vega 11", " CU", "cores"} {
			if strings.Contains(name, banned) {
				t.Errorf("amdModels[%#04x] = %q contains %q: a compute-unit count cannot be derived from a device id", id, name, banned)
			}
		}
	}
}
