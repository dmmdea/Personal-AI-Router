// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package gpunames

import "testing"

// TestParseHexID covers the Linux sysfs form ("3e98", the `device` attribute
// with its "0x" already cut) and every way that read can fail. A malformed
// value must report ok=false rather than parsing to zero, because zero is a
// device id the caller would otherwise look up.
func TestParseHexID(t *testing.T) {
	cases := []struct {
		in   string
		want uint32
		ok   bool
	}{
		{"3e98", 0x3e98, true},
		{"9a60", 0x9a60, true},
		{"15e7", 0x15e7, true},
		{"0b69", 0x0b69, true},
		{"FFFF", 0xffff, true},
		{"0000", 0, true},
		{"", 0, false},
		{"0x3e98", 0, false}, // the prefix is the caller's to strip
		{"zzzz", 0, false},
		{"3e98 ", 0, false},
		{"-1", 0, false},
		{"1ffffffff", 0, false}, // wider than 32 bits
	}
	for _, c := range cases {
		got, ok := ParseHexID(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("ParseHexID(%q) = (%#x, %v), want (%#x, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

// TestDeviceSuffixPadsToFourDigits pins the rendering the string-keyed tables
// used to produce by concatenation. Linux sysfs always prints four hex digits,
// so a row's name has to keep the leading zero - "(device 0xb69)" would not
// match what an operator reads out of lspci.
func TestDeviceSuffixPadsToFourDigits(t *testing.T) {
	cases := []struct {
		id   uint32
		want string
	}{
		{0x3e98, "(device 0x3e98)"},
		{0xffff, "(device 0xffff)"},
		{0x0b69, "(device 0x0b69)"},
		{0x0007, "(device 0x0007)"},
		{0x0000, "(device 0x0000)"},
		{0x123456, "(device 0x123456)"},
	}
	for _, c := range cases {
		if got := deviceSuffix(c.id); got != c.want {
			t.Errorf("deviceSuffix(%#x) = %q, want %q", c.id, got, c.want)
		}
	}
}

// TestPCIVendorConstants pins the two vendor ids the Windows caller gates on.
// A typo here would silently disable the whole lookup, leaving every row on
// DXGI's description with no test failing anywhere.
func TestPCIVendorConstants(t *testing.T) {
	if PCIVendorIntel != 0x8086 {
		t.Errorf("PCIVendorIntel = %#04x, want 0x8086", PCIVendorIntel)
	}
	if PCIVendorAMD != 0x1002 {
		t.Errorf("PCIVendorAMD = %#04x, want 0x1002", PCIVendorAMD)
	}
}

// TestFallbacksAreBareVendorNames: a row that could not be identified must not
// pretend to be one that could, so neither fallback may carry a codename, an
// architecture or a model number.
func TestFallbacksAreBareVendorNames(t *testing.T) {
	if IntelFallback != "Intel Graphics" {
		t.Errorf("IntelFallback = %q, want %q", IntelFallback, "Intel Graphics")
	}
	if AMDFallback != "AMD Radeon Graphics" {
		t.Errorf("AMDFallback = %q, want %q", AMDFallback, "AMD Radeon Graphics")
	}
}
