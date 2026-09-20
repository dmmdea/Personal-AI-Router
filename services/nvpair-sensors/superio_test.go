// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"testing"

	"nvpair-shared/hostsensors"
)

// fakeChip is a Nuvoton monitor window in memory. It speaks the chip's real
// protocol — index port, bank select through register 0x4E, data port — so a
// test drives the same code path a live chip does and a decoding change that
// forgets the bank switch fails here rather than on the bench.
type fakeChip struct {
	base uint16
	// regs is the register file, addressed bank<<8 | register.
	regs map[uint16]byte
	// index is the register the index port currently points at.
	index byte
	// bank is the selected bank.
	bank byte
	// ops records every port operation, so a test can assert the sequence.
	ops []string
	// failPort makes one port fail, for the error-propagation tests.
	failPort uint16
	failErr  error
}

func newFakeChip(base uint16, regs map[uint16]byte) *fakeChip {
	return &fakeChip{base: base, regs: regs}
}

func (c *fakeChip) inb(port uint16) (byte, error) {
	if c.failErr != nil && port == c.failPort {
		return 0, c.failErr
	}
	c.ops = append(c.ops, fmt.Sprintf("in 0x%04X", port))
	if port != c.base+hwmDataOffset {
		return 0xFF, nil
	}
	if c.index == hwmBankSelect {
		return c.bank, nil
	}
	return c.regs[uint16(c.bank)<<8|uint16(c.index)], nil
}

func (c *fakeChip) outb(port uint16, value byte) error {
	if c.failErr != nil && port == c.failPort {
		return c.failErr
	}
	c.ops = append(c.ops, fmt.Sprintf("out 0x%04X=0x%02X", port, value))
	switch port {
	case c.base + hwmAddressOffset:
		c.index = value
	case c.base + hwmDataOffset:
		if c.index == hwmBankSelect {
			c.bank = value
			return nil
		}
		c.regs[uint16(c.bank)<<8|uint16(c.index)] = value
	}
	return nil
}

// nct6798Registers is a captured-shape register file for an NCT6798D on a
// board with three thermistors wired, one PCH readout published only through
// its mirror register, two fans spinning and one header empty.
func nct6798Registers() map[uint16]byte {
	return map[uint16]byte{
		// Which input feeds each switchable register.
		0x100: byte(srcPECI0),
		0x200: byte(srcCPUTIN),
		0x300: byte(srcSYSTIN),
		0x800: byte(srcAUXTIN0),
		0x900: byte(srcAUXTIN1),
		0xA00: byte(srcAUXTIN2),
		0xB00: byte(srcAUXTIN3),
		0x621: byte(srcAUXTIN4),
		0xC00: byte(srcTSensor),
		0x622: byte(srcSMBusMaster0),
		0xC26: byte(srcSMBusMaster1),
		0xC27: byte(srcPECI1),
		0xC28: byte(srcPCHChipCPUMax),
		0xC29: byte(srcPCHChip),
		0xC2A: byte(srcPCHCPU),
		0xC2B: byte(srcPCHMCH),

		// PECI 0: 45 °C with the half-degree bit set -> 45.5.
		0x073: 45, 0x074: 0x80,
		// CPU socket: 40.0.
		0x075: 40, 0x076: 0x00,
		// Motherboard: 30.5.
		0x077: 30, 0x078: 0x80,
		// AUXTIN0 floats at -128 -> not wired.
		0x079: 0x80, 0x07A: 0x00,
		// AUXTIN1..3 and AUXTIN4 read zero -> not wired.
		// T_Sensor: 42.5.
		0x4A2: 42, 0x4A1: 0x80,
		// The PCH chip's switchable register reads nothing; its mirror
		// register at 0x401 carries 50 °C.
		0x676: 0x00, 0x401: 50,

		// Fan 1: count 1227 -> 1100 RPM. Fan 2: count 2455 -> 550 RPM.
		0x4B0: 38, 0x4B1: 0x0B,
		0x4B2: 76, 0x4B3: 0x17,
		// Fan 3: an empty header sits at full scale -> a real 0 RPM.
		0x4B4: 0xFF, 0x4B5: 0xFF,
		// Fan 4: a count below what the conversion expresses -> no row.
		0x4B6: 0x00, 0x4B7: 0x05,

		// Vcore: 140 steps of 8 mV -> 1.12 V.
		0x480: 140,

		// Nuvoton's vendor id, split across two banks of register 0x4F.
		0x804F: 0x5C, 0x004F: 0xA3,
	}
}

func chipOrFail(t *testing.T, id, revision byte) superIOChip {
	t.Helper()
	chip, ok := superIOChipFor(id, revision)
	if !ok {
		t.Fatalf("superIOChipFor(0x%02X, 0x%02X) reported unsupported", id, revision)
	}
	return chip
}

// TestSuperIOChipForIdentifiesTheNuvotonFamily pins the identity table: the
// id/revision pair a chip reports has to resolve to the part whose register
// map this build decodes, and anything else has to be refused rather than
// decoded against a neighbouring part's map.
func TestSuperIOChipForIdentifiesTheNuvotonFamily(t *testing.T) {
	supported := []struct {
		id, revision byte
		name         string
		fans         int
		hasTSensor   bool
		hasAUXTIN4   bool
	}{
		{0xC8, 0x03, "Nuvoton NCT6791D", 6, false, false},
		{0xC9, 0x11, "Nuvoton NCT6792D", 6, false, false},
		{0xC9, 0x13, "Nuvoton NCT6792D-A", 6, false, false},
		{0xD1, 0x21, "Nuvoton NCT6793D", 6, false, false},
		{0xD3, 0x52, "Nuvoton NCT6795D", 6, false, false},
		{0xD4, 0x23, "Nuvoton NCT6796D", 6, false, true},
		{0xD4, 0x2A, "Nuvoton NCT6796D-R", 7, false, true},
		{0xD4, 0x51, "Nuvoton NCT6797D", 7, false, true},
		{0xD4, 0x2B, "Nuvoton NCT6798D", 7, true, true},
		{0xD8, 0x02, "Nuvoton NCT6799D", 7, true, true},
	}
	for _, tc := range supported {
		chip := chipOrFail(t, tc.id, tc.revision)
		if chip.Name != tc.name {
			t.Errorf("0x%02X/0x%02X = %q, want %q", tc.id, tc.revision, chip.Name, tc.name)
		}
		if chip.fans != tc.fans {
			t.Errorf("%s fans = %d, want %d", chip.Name, chip.fans, tc.fans)
		}
		has := func(want tempSource) bool {
			for _, in := range chip.temps {
				if in.source == want {
					return true
				}
			}
			return false
		}
		if has(srcTSensor) != tc.hasTSensor {
			t.Errorf("%s T_Sensor input present = %v, want %v", chip.Name, has(srcTSensor), tc.hasTSensor)
		}
		if has(srcAUXTIN4) != tc.hasAUXTIN4 {
			t.Errorf("%s AUXTIN4 input present = %v, want %v", chip.Name, has(srcAUXTIN4), tc.hasAUXTIN4)
		}
		if !has(srcCPUTIN) || !has(srcSYSTIN) {
			t.Errorf("%s is missing CPUTIN or SYSTIN", chip.Name)
		}
	}

	// A neighbouring revision, an ITE chip's id, and the two answers an
	// absent chip gives must all be refused.
	for _, tc := range []struct{ id, revision byte }{
		{0xD4, 0x99}, {0xC7, 0x32}, {0xD5, 0x92}, {0x87, 0x12}, {0x00, 0x00}, {0xFF, 0xFF},
	} {
		if chip, ok := superIOChipFor(tc.id, tc.revision); ok {
			t.Errorf("0x%02X/0x%02X resolved to %q, want unsupported", tc.id, tc.revision, chip.Name)
		}
	}
}

// TestHWMWindowReadByteSelectsBankThenRegister pins the access protocol: a
// read has to select the bank through index 0x4E before it points the index
// port at the register, or every read above bank 0 returns bank 0's value.
func TestHWMWindowReadByteSelectsBankThenRegister(t *testing.T) {
	chip := newFakeChip(0x0290, map[uint16]byte{0x4A2: 42, 0x0A2: 99})
	w := hwmWindow{io: chip, base: 0x0290}

	got, err := w.readByte(0x4A2)
	if err != nil {
		t.Fatal(err)
	}
	if got != 42 {
		t.Fatalf("readByte(0x4A2) = %d, want 42 (bank 4, not bank 0)", got)
	}
	want := []string{
		"out 0x0295=0x4E",
		"out 0x0296=0x04",
		"out 0x0295=0xA2",
		"in 0x0296",
	}
	if len(chip.ops) != len(want) {
		t.Fatalf("port operations = %v, want %v", chip.ops, want)
	}
	for i := range want {
		if chip.ops[i] != want[i] {
			t.Fatalf("port operations = %v, want %v", chip.ops, want)
		}
	}
}

// TestHWMWindowVendorIDReadsBothBanks: the id is the proof the window is
// open, and it only reads back correctly if both halves come from their own
// bank.
func TestHWMWindowVendorIDReadsBothBanks(t *testing.T) {
	w := hwmWindow{io: newFakeChip(0x0290, map[uint16]byte{0x804F: 0x5C, 0x004F: 0xA3}), base: 0x0290}
	id, err := w.vendorID()
	if err != nil {
		t.Fatal(err)
	}
	if id != hwmVendorNuvoton {
		t.Fatalf("vendorID = 0x%04X, want 0x%04X", id, hwmVendorNuvoton)
	}

	// A locked window answers 0xFF everywhere, which must not look like a
	// vendor id.
	locked := hwmWindow{io: newFakeChip(0x0290, map[uint16]byte{0x804F: 0xFF, 0x004F: 0xFF}), base: 0x0290}
	if id, err = locked.vendorID(); err != nil {
		t.Fatal(err)
	}
	if id == hwmVendorNuvoton {
		t.Fatal("a locked window must not report Nuvoton's vendor id")
	}
}

// TestDecodeTemperaturesReadsTheWiredInputs is the end-to-end decode against
// the captured register file: the half-degree bit is folded in, the source
// register decides which input a register is carrying, an unwired input is
// dropped rather than published as a number, and an input that only the
// mirror register carries is still found.
func TestDecodeTemperaturesReadsTheWiredInputs(t *testing.T) {
	chip := chipOrFail(t, 0xD4, 0x2B) // NCT6798D
	w := hwmWindow{io: newFakeChip(0x0290, nct6798Registers()), base: 0x0290}

	temps, err := decodeTemperatures(chip, w.readByte)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]hostsensors.BoardTemp{}
	for _, tp := range temps {
		if _, dup := got[tp.Input]; dup {
			t.Fatalf("input %s reported twice: %v", tp.Input, temps)
		}
		got[tp.Input] = tp
	}

	for _, want := range []hostsensors.BoardTemp{
		{Label: "PECI 0", Input: "PECI_0", Role: hostsensors.RolePECI, Celsius: 45.5},
		{Label: "CPU socket", Input: "CPUTIN", Role: hostsensors.RoleCPUSocket, Celsius: 40},
		{Label: "Motherboard", Input: "SYSTIN", Role: hostsensors.RoleMotherboard, Celsius: 30.5},
		{Label: "T_Sensor", Input: "TSENSOR", Role: hostsensors.RoleAux, Celsius: 42.5},
		{Label: "PCH chip", Input: "PCH_CHIP_TEMP", Role: hostsensors.RolePCH, Celsius: 50},
	} {
		if got[want.Input] != want {
			t.Errorf("input %s = %+v, want %+v", want.Input, got[want.Input], want)
		}
	}
	// AUXTIN0 floats negative and AUXTIN1..4 read zero: none is a sensor.
	for _, absent := range []string{"AUXTIN0", "AUXTIN1", "AUXTIN2", "AUXTIN3", "AUXTIN4"} {
		if tp, present := got[absent]; present {
			t.Errorf("unwired input %s was published as %.1f °C", absent, tp.Celsius)
		}
	}
	if len(temps) != 5 {
		t.Fatalf("published %d temperatures, want 5: %+v", len(temps), temps)
	}
}

// TestDecodeTemperaturesKeepsTheFirstRegisterToClaimAnInput: when two
// registers report the same input, the first one wins, because the later ones
// carry whole degrees only and would silently drop the half-degree bit.
func TestDecodeTemperaturesKeepsTheFirstRegisterToClaimAnInput(t *testing.T) {
	regs := nct6798Registers()
	// Point a second switchable register at CPUTIN as well, carrying a
	// different, coarser value.
	regs[0x800] = byte(srcCPUTIN)
	regs[0x079] = 55
	regs[0x07A] = 0x00

	chip := chipOrFail(t, 0xD4, 0x2B)
	temps, err := decodeTemperatures(chip, hwmWindow{io: newFakeChip(0x0290, regs), base: 0x0290}.readByte)
	if err != nil {
		t.Fatal(err)
	}
	var seen int
	for _, tp := range temps {
		if tp.Input != "CPUTIN" {
			continue
		}
		seen++
		if tp.Celsius != 40 {
			t.Fatalf("CPUTIN = %.1f °C, want the first register's 40", tp.Celsius)
		}
	}
	if seen != 1 {
		t.Fatalf("CPUTIN reported %d times, want once", seen)
	}
}

// TestDecodeTemperaturesRejectsALockedWindow: every register of a locked
// window reads 0xFF, which decodes to -0.5 °C. Publishing that as a row of
// plausible-looking sensors is exactly the failure the range rule prevents.
func TestDecodeTemperaturesRejectsALockedWindow(t *testing.T) {
	regs := map[uint16]byte{}
	for addr := 0; addr <= 0xFFF; addr++ {
		regs[uint16(addr)] = 0xFF
	}
	chip := chipOrFail(t, 0xD4, 0x2B)
	temps, err := decodeTemperatures(chip, hwmWindow{io: newFakeChip(0x0290, regs), base: 0x0290}.readByte)
	if err != nil {
		t.Fatal(err)
	}
	if len(temps) != 0 {
		t.Fatalf("a locked window published %d temperatures: %+v", len(temps), temps)
	}
}

// TestDecodeTemperaturesPropagatesAReadFailure: a driver that stops answering
// must surface as an error, never as a short list of readings.
func TestDecodeTemperaturesPropagatesAReadFailure(t *testing.T) {
	chip := chipOrFail(t, 0xD4, 0x2B)
	fake := newFakeChip(0x0290, nct6798Registers())
	fake.failPort, fake.failErr = 0x0296, errors.New("driver gone")
	if _, err := decodeTemperatures(chip, hwmWindow{io: fake, base: 0x0290}.readByte); err == nil {
		t.Fatal("expected the read failure to propagate")
	}
}

// TestDecodeFanRPM pins the 13-bit tachometer maths, including the two
// boundaries that are not readings: full scale is a stopped header (a real
// zero), and a count below the conversion's floor is no reading at all.
func TestDecodeFanRPM(t *testing.T) {
	cases := []struct {
		name       string
		high, low  byte
		wantRPM    uint32
		wantUsable bool
	}{
		{"1100 rpm", 38, 0x0B, 1100, true},
		{"550 rpm", 76, 0x17, 550, true},
		{"the low byte's upper bits are not part of the count", 38, 0xEB, 1100, true},
		{"full scale is a stopped header", 0xFF, 0xFF, 0, true},
		{"a count under the floor is not a reading", 0x00, 0x05, 0, false},
		{"the slowest expressible count", 0x00, 0x15, 64286, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rpm, ok := decodeFanRPM(tc.high, tc.low)
			if ok != tc.wantUsable {
				t.Fatalf("usable = %v, want %v", ok, tc.wantUsable)
			}
			if ok && rpm != tc.wantRPM {
				t.Fatalf("rpm = %d, want %d", rpm, tc.wantRPM)
			}
		})
	}
}

// TestDecodeFansReadsEveryChannelTheChipHas: the channel count follows the
// part, an empty header is a real 0 RPM row, and an unusable count produces
// no row at all.
func TestDecodeFansReadsEveryChannelTheChipHas(t *testing.T) {
	chip := chipOrFail(t, 0xD4, 0x2B) // NCT6798D: seven channels
	fans, err := decodeFans(chip, hwmWindow{io: newFakeChip(0x0290, nct6798Registers()), base: 0x0290}.readByte)
	if err != nil {
		t.Fatal(err)
	}
	want := []hostsensors.BoardFan{
		{Index: 0, Label: "Fan 1", RPM: 1100},
		{Index: 1, Label: "Fan 2", RPM: 550},
		{Index: 2, Label: "Fan 3", RPM: 0},
	}
	if len(fans) != len(want) {
		t.Fatalf("fans = %+v, want %+v", fans, want)
	}
	for i := range want {
		if fans[i] != want[i] {
			t.Fatalf("fans = %+v, want %+v", fans, want)
		}
	}

	// A six-channel part must not read the seventh channel's register.
	six := chipOrFail(t, 0xD4, 0x23) // NCT6796D
	fake := newFakeChip(0x0290, nct6798Registers())
	if _, err := decodeFans(six, hwmWindow{io: fake, base: 0x0290}.readByte); err != nil {
		t.Fatal(err)
	}
	for _, op := range fake.ops {
		if op == "out 0x0295=0xCC" {
			t.Fatal("a six-channel chip read the seventh tachometer register 0x4CC")
		}
	}
}

// TestDecodeVcore pins the 8 mV step of the analogue inputs.
func TestDecodeVcore(t *testing.T) {
	w := hwmWindow{io: newFakeChip(0x0290, nct6798Registers()), base: 0x0290}
	v, err := decodeVcore(w.readByte)
	if err != nil {
		t.Fatal(err)
	}
	if v < 1.1195 || v > 1.1205 {
		t.Fatalf("Vcore = %v V, want 1.12", v)
	}
}

// TestInvalidMonitorBase pins the base-address sanity check: the two answers
// a disabled logical device gives, an address inside the fixed I/O range, and
// one that is not on the window's 8-byte boundary are all refusals.
func TestInvalidMonitorBase(t *testing.T) {
	for _, tc := range []struct {
		address uint16
		invalid bool
	}{
		{0x0290, false},
		{0x0A20, false},
		{0x0000, true},
		{0xFFFF, true},
		{0x00F8, true},
		{0x0291, true},
		{0xF290, true},
	} {
		if got := invalidMonitorBase(tc.address); got != tc.invalid {
			t.Errorf("invalidMonitorBase(0x%04X) = %v, want %v", tc.address, got, tc.invalid)
		}
	}
}

// TestNoInputClaimsTheVRMRoleFromTheChipAlone pins the rule the measurement on
// the target board forced: a Super I/O chip cannot tell you whether a probe
// header has a probe in it, so no input may claim RoleVRM on the chip's word.
// The ASUS T_Sensor input there declared itself connected and reported a flat
// 88 °C that did not move under load; if this test ever fails because a source
// gained RoleVRM, that source needs evidence outside the chip behind it.
func TestNoInputClaimsTheVRMRoleFromTheChipAlone(t *testing.T) {
	for source, name := range tempSourceNames {
		if name.role == hostsensors.RoleVRM {
			t.Errorf("input %s (source %d) claims the VRM role from the chip alone", name.input, source)
		}
	}
}

// TestChipIDString pins the identity spelling a field report quotes.
func TestChipIDString(t *testing.T) {
	if got := chipIDString(0xD4, 0x2B); got != "0xD4/0x2B" {
		t.Fatalf("chipIDString = %q", got)
	}
}
