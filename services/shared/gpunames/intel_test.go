// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package gpunames

import (
	"strings"
	"testing"
)

// TestIntel walks the table across every generation it covers, including the
// three ids where the obvious guess is wrong (0x9a60 is UHD, not Iris Xe;
// 0x9a78 is UHD Graphics G4 although it is a GT2 part; 0x7dd5 and 0x7d55 are
// the same generation under two names).
func TestIntel(t *testing.T) {
	cases := []struct {
		id   uint32
		want string
	}{
		{0x3e90, "Intel UHD Graphics 610 (Coffee Lake, Gen 9.5)"},
		{0x3e98, "Intel UHD Graphics 630 (Coffee Lake, Gen 9.5)"},
		{0x3e9b, "Intel UHD Graphics 630 (Coffee Lake, Gen 9.5)"},
		{0x3e96, "Intel UHD Graphics P630 (Coffee Lake, Gen 9.5)"},
		{0x3ea0, "Intel UHD Graphics 620 (Whiskey Lake, Gen 9.5)"},
		{0x3ea1, "Intel UHD Graphics 610 (Whiskey Lake, Gen 9.5)"},
		{0x87ca, "Intel UHD Graphics 617 (Amber Lake, Gen 9.5)"},
		{0x9b41, "Intel UHD Graphics (Comet Lake, Gen 9.5)"},
		{0x9bc5, "Intel UHD Graphics 630 (Comet Lake, Gen 9.5)"},
		{0x9bca, "Intel UHD Graphics (Comet Lake, Gen 9.5)"},
		{0x4c8a, "Intel UHD Graphics 750 (Rocket Lake, Xe-LP)"},
		{0x9a60, "Intel UHD Graphics (Tiger Lake, Xe-LP)"},
		{0x9a68, "Intel UHD Graphics (Tiger Lake, Xe-LP)"},
		{0x9a70, "Intel UHD Graphics (Tiger Lake, Xe-LP)"},
		{0x9a40, "Intel Iris Xe Graphics (Tiger Lake, Xe-LP)"},
		{0x9a49, "Intel Iris Xe Graphics (Tiger Lake, Xe-LP)"},
		{0x9ac0, "Intel Iris Xe Graphics (Tiger Lake, Xe-LP)"},
		{0x9a78, "Intel UHD Graphics G4 (Tiger Lake, Xe-LP)"},
		{0x4905, "Intel Iris Xe MAX Graphics (DG1, Xe-LP)"},
		{0x4680, "Intel UHD Graphics 770 (Alder Lake, Xe-LP)"},
		{0x4693, "Intel UHD Graphics 710 (Alder Lake, Xe-LP)"},
		{0x46a6, "Intel Iris Xe Graphics (Alder Lake, Xe-LP)"},
		{0x46d3, "Intel Graphics (Alder Lake, Xe-LP)"},
		{0xa780, "Intel UHD Graphics 770 (Raptor Lake, Xe-LP)"},
		{0xa789, "Intel UHD Graphics (Raptor Lake, Xe-LP)"},
		{0xa7a1, "Intel Iris Xe Graphics (Raptor Lake, Xe-LP)"},
		{0x56a0, "Intel Arc A770 (Alchemist, Xe-HPG)"},
		{0x56a1, "Intel Arc A750 (Alchemist, Xe-HPG)"},
		{0x5690, "Intel Arc A770M (Alchemist, Xe-HPG)"},
		{0x56c0, "Intel Data Center GPU Flex 170 (ATS-M, Xe-HPG)"},
		{0x7d55, "Intel Arc Graphics (Meteor Lake, Xe-LPG)"},
		{0x7dd5, "Intel Graphics (Meteor Lake, Xe-LPG)"},
		{0x7d67, "Intel Graphics (Arrow Lake, Xe-LPG)"},
		{0x64a0, "Intel Arc Graphics 130V/140V (Lunar Lake, Xe2)"},
		{0xe20b, "Intel Arc B580 (Battlemage, Xe2-HPG)"},
		{0xe20c, "Intel Arc B570 (Battlemage, Xe2-HPG)"},
	}
	for _, c := range cases {
		got, ok := Intel(c.id)
		if !ok {
			t.Errorf("Intel(%#04x) reported unknown, want %q", c.id, c.want)
			continue
		}
		if got != c.want {
			t.Errorf("Intel(%#04x) = %q, want %q", c.id, got, c.want)
		}
	}
}

// TestIntelUnknownID: an id the table does not carry must report ok=false
// rather than an empty name, because the Windows caller uses that signal to
// keep DXGI's own description instead of publishing "".
func TestIntelUnknownID(t *testing.T) {
	for _, id := range []uint32{0x0000, 0xabcd, 0xffff, 0x1234} {
		if name, ok := Intel(id); ok {
			t.Errorf("Intel(%#04x) = %q, want unknown", id, name)
		}
	}
}

// TestIntelName covers the fallback ladder the Linux inventory depends on: a
// listed id keeps its full name, an unlisted one keeps its device id, and an
// id that could not be read at all becomes the bare vendor name.
func TestIntelName(t *testing.T) {
	cases := []struct {
		id     uint32
		haveID bool
		want   string
	}{
		{0x3e98, true, "Intel UHD Graphics 630 (Coffee Lake, Gen 9.5)"},
		{0x9a60, true, "Intel UHD Graphics (Tiger Lake, Xe-LP)"},
		{0xabcd, true, "Intel Graphics (device 0xabcd)"},
		{0xffff, true, "Intel Graphics (device 0xffff)"},
		{0x0b69, true, "Intel Graphics (device 0x0b69)"},
		{0x0000, false, "Intel Graphics"},
		{0x3e98, false, "Intel UHD Graphics 630 (Coffee Lake, Gen 9.5)"},
	}
	for _, c := range cases {
		if got := IntelName(c.id, c.haveID); got != c.want {
			t.Errorf("IntelName(%#04x, %v) = %q, want %q", c.id, c.haveID, got, c.want)
		}
	}
}

// TestIntelDiscrete pins the memory verdict the table carries: every "Lake" is
// an integrated part that allocates out of system DRAM, and every DG1 / Arc /
// Battlemage part has its own VRAM. An unlisted id must report known=false so
// the caller falls through to judging the card by its own sysfs numbers.
func TestIntelDiscrete(t *testing.T) {
	cases := []struct {
		id       uint32
		discrete bool
		known    bool
	}{
		{0x3e98, false, true},  // Coffee Lake iGPU
		{0x9a60, false, true},  // Tiger Lake iGPU
		{0xa780, false, true},  // Raptor Lake iGPU
		{0x7d55, false, true},  // Meteor Lake iGPU
		{0x64a0, false, true},  // Lunar Lake iGPU
		{0x4905, true, true},   // DG1
		{0x56a0, true, true},   // Arc A770
		{0x56c0, true, true},   // Data Center GPU Flex 170
		{0xe20b, true, true},   // Arc B580
		{0xffff, false, false}, // unlisted
	}
	for _, c := range cases {
		discrete, known := IntelDiscrete(c.id)
		if discrete != c.discrete || known != c.known {
			t.Errorf("IntelDiscrete(%#04x) = (%v, %v), want (%v, %v)",
				c.id, discrete, known, c.discrete, c.known)
		}
	}
}

// TestIntelNamesTheGeneration mirrors the AMD rule, and moved here with the
// table it walks: a node list has to say which graphics generation the part
// is, because that is what decides what it can run. A bare marketing name does
// not. It also pins the shape, so a new entry cannot quietly drop the codename
// or the vendor prefix.
func TestIntelNamesTheGeneration(t *testing.T) {
	architectures := []string{"Gen 9.5", "Xe-LP", "Xe-HPG", "Xe-LPG", "Xe2", "Xe2-HPG"}
	for id := range intelModels {
		name := IntelName(id, true)
		named := false
		for _, arch := range architectures {
			if strings.HasSuffix(name, ", "+arch+")") {
				named = true
				break
			}
		}
		if !named {
			t.Errorf("intelModels[%#04x] = %q; every name must end in a known architecture %v", id, name, architectures)
		}
		if !strings.HasPrefix(name, "Intel ") {
			t.Errorf("intelModels[%#04x] = %q; want a vendor-prefixed name", id, name)
		}
		if !strings.Contains(name, " (") {
			t.Errorf("intelModels[%#04x] = %q; want the \"<name> (<codename>, <architecture>)\" shape", id, name)
		}
		// The generic fallback shape must never appear inside the table: a
		// listed part is one we can name, so a device id in the string would
		// mean the entry was copied from a fallback rather than sourced.
		if strings.Contains(name, "(device 0x") {
			t.Errorf("intelModels[%#04x] = %q carries a raw device id; a listed part must be named", id, name)
		}
	}
}

// TestIntelTableCoversTheMeasuredHosts is the regression this package exists
// for: the two boxes whose rows disagreed across platforms. The laptop's
// Tiger Lake i7 (0x9a60, GT1) used to publish DXGI's bare "Intel(R) UHD
// Graphics" on Windows while Linux named the generation, and the desktop's
// Coffee Lake i7 (0x3e98) published "Intel(R) UHD Graphics 630" with no
// generation at all.
func TestIntelTableCoversTheMeasuredHosts(t *testing.T) {
	for id, want := range map[uint32]string{
		0x9a60: "Intel UHD Graphics (Tiger Lake, Xe-LP)",
		0x3e98: "Intel UHD Graphics 630 (Coffee Lake, Gen 9.5)",
	} {
		got, ok := Intel(id)
		if !ok || got != want {
			t.Errorf("Intel(%#04x) = (%q, %v), want (%q, true)", id, got, ok, want)
		}
	}
}
