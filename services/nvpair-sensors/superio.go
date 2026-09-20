// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// Nuvoton NCT67xx Super I/O hardware-monitor decoding.
//
// A PC motherboard's own sensors — the socket-side and board thermistors, the
// chipset's readouts, the tachometers and the analogue voltage inputs — live
// in a Super I/O chip on the LPC bus, not in any OS-visible device. Reaching
// it is two index/data port pairs and nothing else: a configuration window at
// 0x2E/0x2F (or 0x4E/0x4F) that identifies the chip and hands out the base
// address of its hardware-monitor window, then that window itself, where a
// bank byte plus a register byte address every sensor.
//
// Everything in this file is the bit maths and the register maps, kept free
// of any platform API so it is unit tested on every host. The port I/O
// underneath is a Windows ring-0 call and lives in board_windows.go.
//
// The register maps and the decoding follow LibreHardwareMonitor's Nct677X /
// LpcIO implementation, which is the reference for this family; the chip
// identity table is its Winbond/Nuvoton/Fintek detection switch.

import (
	"fmt"
	"math"

	"nvpair-shared/hostsensors"
)

// sourceSuperIO names the mechanism in a published reading.
const sourceSuperIO = "superio-lpc"

// Configuration-window ports. Slot 0 is the primary Super I/O, slot 1 the
// secondary one some boards fit.
const (
	configPortPrimary   uint16 = 0x2E
	configPortSecondary uint16 = 0x4E
)

// Configuration-window registers and the magic bytes that open and close it.
const (
	cfgChipID       byte = 0x20 // chip identity, high half
	cfgChipRevision byte = 0x21 // chip identity, low half
	cfgDeviceSelect byte = 0x07 // which logical device the window addresses
	cfgBaseAddress  byte = 0x60 // the selected device's base address (word)
	cfgIOSpaceLock  byte = 0x28 // Nuvoton: bit 4 locks the monitor window

	// cfgEnter is written twice to the index port to open the configuration
	// window; cfgExit closes it. Leaving it open lets any other tool's
	// unrelated port write land on this chip's configuration.
	cfgEnter byte = 0x87
	cfgExit  byte = 0xAA

	// hwmLogicalDevice is the logical device number the hardware monitor
	// lives behind on every Winbond/Nuvoton part.
	hwmLogicalDevice byte = 0x0B

	// cfgIOSpaceLockBit is the lock bit inside cfgIOSpaceLock. While it is
	// set the monitor window reads back as 0xFF.
	cfgIOSpaceLockBit byte = 0x10
)

// Hardware-monitor window layout: an index/data pair at base+5 / base+6, with
// index 0x4E selecting which 256-byte bank the following register byte
// addresses. A sensor address in this file is therefore bank<<8 | register.
const (
	hwmAddressOffset uint16 = 0x05
	hwmDataOffset    uint16 = 0x06
	hwmBankSelect    byte   = 0x4E

	// hwmVendorIDHigh / hwmVendorIDLow hold Nuvoton's id, split across two
	// banks of the same register. Reading it back is how this code confirms
	// the window is really open rather than returning 0xFF.
	hwmVendorIDHigh uint16 = 0x804F
	hwmVendorIDLow  uint16 = 0x004F
	hwmVendorNuvoton uint16 = 0x5CA3
)

// portIO is one byte-wide I/O port pair of operations. The Windows
// implementation routes both through the signed PawnIO LpcIO module, which
// refuses any port outside the chip's own window; tests substitute a map of
// captured register bytes.
type portIO interface {
	inb(port uint16) (byte, error)
	outb(port uint16, value byte) error
}

// tempSource is the chip's own numbering of its temperature inputs. A
// monitor register does not have a fixed meaning: a companion "source"
// register says which input currently feeds it, so the same register reads a
// different sensor depending on how the board's firmware wired the chip up.
type tempSource byte

const (
	srcSYSTIN        tempSource = 1
	srcCPUTIN        tempSource = 2
	srcAUXTIN0       tempSource = 3
	srcAUXTIN1       tempSource = 4
	srcAUXTIN2       tempSource = 5
	srcAUXTIN3       tempSource = 6
	srcAUXTIN4       tempSource = 7
	srcSMBusMaster0  tempSource = 8
	srcSMBusMaster1  tempSource = 9
	srcTSensor       tempSource = 10
	srcPECI0         tempSource = 16
	srcPECI1         tempSource = 17
	srcPCHChipCPUMax tempSource = 18
	srcPCHChip       tempSource = 19
	srcPCHCPU        tempSource = 20
	srcPCHMCH        tempSource = 21
	srcAgent0DIMM0   tempSource = 22
	srcAgent0DIMM1   tempSource = 23
	srcAgent1DIMM0   tempSource = 24
	srcAgent1DIMM1   tempSource = 25
	srcByteTemp0     tempSource = 26
	srcByteTemp1     tempSource = 27
	srcPECI0Cal      tempSource = 28
	srcPECI1Cal      tempSource = 29
	srcVirtual       tempSource = 31
)

// tempSourceName pairs an input's human label with what it measures.
type tempSourceName struct {
	label string
	input string
	role  string
}

// tempSourceNames labels every input this decoder can emit.
//
// Role assignment is deliberately conservative, and the reason is measured.
// CPUTIN and SYSTIN mean the same thing on every board in this family, and the
// PECI and PCH readouts come from the CPU and chipset themselves, so those
// carry a role a reader can act on.
//
// Everything else — the AUXTINs and TSENSOR — is RoleAux, including the input
// ASUS brings out as its T_Sensor / VRM probe header. That header is a place
// to plug a thermistor in, not a sensor, and the chip cannot tell you whether
// anyone did: on the ASUS ROG STRIX X299-E GAMING II (NCT6798D, BIOS 2103)
// the chip's own source register declares TSENSOR connected, while the input
// reports a flat 88 °C that does not move between idle and eight threads of
// load — and reads byte-for-byte identical to AUXTIN4 across every sample.
// Calling that "the board's VRM temperature" would have put a fabricated
// 88 °C on the node card. Which AUXTIN a given board wired to its VRM, its
// chipset, or nothing at all is a board-level decision nothing in the chip
// reports, so none of them claims RoleVRM until something outside the chip
// can say so.
var tempSourceNames = map[tempSource]tempSourceName{
	srcSYSTIN:        {"Motherboard", "SYSTIN", hostsensors.RoleMotherboard},
	srcCPUTIN:        {"CPU socket", "CPUTIN", hostsensors.RoleCPUSocket},
	srcAUXTIN0:       {"Aux 0", "AUXTIN0", hostsensors.RoleAux},
	srcAUXTIN1:       {"Aux 1", "AUXTIN1", hostsensors.RoleAux},
	srcAUXTIN2:       {"Aux 2", "AUXTIN2", hostsensors.RoleAux},
	srcAUXTIN3:       {"Aux 3", "AUXTIN3", hostsensors.RoleAux},
	srcAUXTIN4:       {"Aux 4", "AUXTIN4", hostsensors.RoleAux},
	srcSMBusMaster0:  {"SMBus 0", "SMBUSMASTER0", hostsensors.RoleSMBus},
	srcSMBusMaster1:  {"SMBus 1", "SMBUSMASTER1", hostsensors.RoleSMBus},
	srcTSensor:       {"T_Sensor", "TSENSOR", hostsensors.RoleAux},
	srcPECI0:         {"PECI 0", "PECI_0", hostsensors.RolePECI},
	srcPECI1:         {"PECI 1", "PECI_1", hostsensors.RolePECI},
	srcPCHChipCPUMax: {"PCH chip CPU max", "PCH_CHIP_CPU_MAX_TEMP", hostsensors.RolePCH},
	srcPCHChip:       {"PCH chip", "PCH_CHIP_TEMP", hostsensors.RolePCH},
	srcPCHCPU:        {"PCH CPU", "PCH_CPU_TEMP", hostsensors.RolePCH},
	srcPCHMCH:        {"PCH MCH", "PCH_MCH_TEMP", hostsensors.RolePCH},
	srcAgent0DIMM0:   {"DIMM 0-0", "AGENT0_DIMM0", hostsensors.RoleDIMM},
	srcAgent0DIMM1:   {"DIMM 0-1", "AGENT0_DIMM1", hostsensors.RoleDIMM},
	srcAgent1DIMM0:   {"DIMM 1-0", "AGENT1_DIMM0", hostsensors.RoleDIMM},
	srcAgent1DIMM1:   {"DIMM 1-1", "AGENT1_DIMM1", hostsensors.RoleDIMM},
	srcByteTemp0:     {"Device 0", "BYTE_TEMP0", hostsensors.RoleAux},
	srcByteTemp1:     {"Device 1", "BYTE_TEMP1", hostsensors.RoleAux},
	srcPECI0Cal:      {"PECI 0 calibrated", "PECI_0_CAL", hostsensors.RolePECI},
	srcPECI1Cal:      {"PECI 1 calibrated", "PECI_1_CAL", hostsensors.RolePECI},
	srcVirtual:       {"Virtual", "VIRTUAL_TEMP", hostsensors.RoleVirtual},
}

// tempInput is one temperature register of a chip, with everything needed to
// decode it:
//
//	reg       the whole-degrees register, two's complement, 1 °C per step
//	halfReg   the register carrying its half-degree bit (halfBit its position)
//	sourceReg the register naming which input feeds reg right now; when it is
//	          zero, the input is fixed and `source` says which
//	altReg    a whole-degrees-only fallback for inputs the firmware also
//	          mirrors into a plain byte, read when reg produced nothing
type tempInput struct {
	source    tempSource
	reg       uint16
	halfReg   uint16
	halfBit   int
	sourceReg uint16
	altReg    uint16
}

// tempMaxCelsius is the top of the range the chip reports (-55..125 °C).
//
// The bottom of the usable range is not the chip's -55 but zero. An input
// with nothing on it floats and decodes to whatever the pin happens to sit
// at, which is routinely a large negative number or an exact zero — and a
// board sensor inside a running PC never reads either. Treating anything at
// or below zero as "not wired" is what keeps a ten-input chip on a
// three-thermistor board from publishing seven inventions.
const tempMaxCelsius = 125.0

// superIOChip is one supported Super I/O part.
type superIOChip struct {
	// Name is the part, e.g. "Nuvoton NCT6798D".
	Name string
	// temps is the chip's temperature register map, in input order.
	temps []tempInput
	// fans is how many tachometer channels the part has.
	fans int
}

// Monitor registers shared by the whole NCT67xxD family.
var (
	// fanCountRegisters hold a 13-bit tachometer count each, high byte at
	// the listed address and low byte at the next one.
	fanCountRegisters = []uint16{0x4B0, 0x4B2, 0x4B4, 0x4B6, 0x4B8, 0x4BA, 0x4CC}
	// vcoreRegister is the first analogue input, 8 mV per step.
	vcoreRegister uint16 = 0x480
)

const (
	// fanMaxCount is the counter's full scale: a header with nothing
	// spinning on it sits there.
	fanMaxCount = 0x1FFF
	// fanMinCount is the smallest count the RPM conversion can express in
	// the 16-bit register the chip family derives it from.
	fanMinCount = 0x15
	// fanCountToRPM converts a tachometer count into revolutions a minute.
	fanCountToRPM = 1.35e6
	// voltsPerStep is the analogue inputs' resolution.
	voltsPerStep = 0.008
)

// tempsNCT6791 is the map shared by the NCT6791D/6792D/6793D/6795D parts:
// the six switchable inputs, the PECI and PCH readouts, and the DIMM agents.
func tempsNCT6791() []tempInput {
	return append(tempsSwitchable(), tempsFixed()...)
}

// tempsNCT6796 adds AUXTIN4 to the switchable set (NCT6796D/6796DR/6797D).
func tempsNCT6796() []tempInput {
	return append(append(tempsSwitchable(),
		tempInput{source: srcAUXTIN4, reg: 0x027, halfBit: -1, sourceReg: 0x621},
	), tempsFixed()...)
}

// tempsNCT6798 adds the T_Sensor input on top of that (NCT6798D/6799D) —
// the probe header ASUS brings out on its boards.
func tempsNCT6798() []tempInput {
	return append(append(tempsSwitchable(),
		tempInput{source: srcAUXTIN4, reg: 0x027, halfBit: -1, sourceReg: 0x621},
		tempInput{source: srcTSensor, reg: 0x4A2, halfReg: 0x4A1, halfBit: 7, sourceReg: 0xC00, altReg: 0x496},
	), tempsFixed()...)
}

// tempsSwitchable is the block of registers whose input is chosen by a
// companion source register — the seven the board's firmware assigns.
func tempsSwitchable() []tempInput {
	return []tempInput{
		{source: srcPECI0, reg: 0x073, halfReg: 0x074, halfBit: 7, sourceReg: 0x100},
		{source: srcCPUTIN, reg: 0x075, halfReg: 0x076, halfBit: 7, sourceReg: 0x200, altReg: 0x491},
		{source: srcSYSTIN, reg: 0x077, halfReg: 0x078, halfBit: 7, sourceReg: 0x300, altReg: 0x490},
		{source: srcAUXTIN0, reg: 0x079, halfReg: 0x07A, halfBit: 7, sourceReg: 0x800, altReg: 0x492},
		{source: srcAUXTIN1, reg: 0x07B, halfReg: 0x07C, halfBit: 7, sourceReg: 0x900, altReg: 0x493},
		{source: srcAUXTIN2, reg: 0x07D, halfReg: 0x07E, halfBit: 7, sourceReg: 0xA00, altReg: 0x494},
		{source: srcAUXTIN3, reg: 0x4A0, halfReg: 0x49E, halfBit: 6, sourceReg: 0xB00, altReg: 0x495},
	}
}

// tempsFixed is the block whose inputs never move: the SMBus-reached
// sensors, the second PECI agent, the chipset's four readouts and the memory
// agents.
func tempsFixed() []tempInput {
	return []tempInput{
		{source: srcSMBusMaster0, reg: 0x150, halfReg: 0x151, halfBit: 7, sourceReg: 0x622},
		{source: srcSMBusMaster1, reg: 0x670, halfBit: -1, sourceReg: 0xC26},
		{source: srcPECI1, reg: 0x672, halfBit: -1, sourceReg: 0xC27},
		{source: srcPCHChipCPUMax, reg: 0x674, halfBit: -1, sourceReg: 0xC28, altReg: 0x400},
		{source: srcPCHChip, reg: 0x676, halfBit: -1, sourceReg: 0xC29, altReg: 0x401},
		{source: srcPCHCPU, reg: 0x678, halfBit: -1, sourceReg: 0xC2A, altReg: 0x402},
		{source: srcPCHMCH, reg: 0x67A, halfBit: -1, sourceReg: 0xC2B, altReg: 0x404},
		{source: srcAgent0DIMM0, reg: 0x405, halfBit: -1},
		{source: srcAgent0DIMM1, reg: 0x406, halfBit: -1},
		{source: srcAgent1DIMM0, reg: 0x407, halfBit: -1},
		{source: srcAgent1DIMM1, reg: 0x408, halfBit: -1},
		{source: srcByteTemp0, reg: 0x419, halfBit: -1},
		{source: srcByteTemp1, reg: 0x41A, halfBit: -1},
		{source: srcPECI0Cal, reg: 0x4F4, halfBit: -1},
		{source: srcPECI1Cal, reg: 0x4F5, halfBit: -1},
	}
}

// superIOChipFor resolves a chip identity to its register map.
//
// The supported set is the NCT67xxD family: the parts that share the
// bank-switched monitor window this file decodes. Deliberately outside it are
// the NCT668x embedded-controller parts (a different register space
// altogether, reached through a page/index/data protocol with its own
// arbitration) and the NCT610x, whose map barely overlaps. A board fitted
// with one of those, or with an ITE or Fintek chip, is reported as
// unsupported rather than decoded against the wrong map.
func superIOChipFor(id, revision byte) (superIOChip, bool) {
	switch id {
	case 0xC8:
		if revision == 0x03 {
			return superIOChip{Name: "Nuvoton NCT6791D", temps: tempsNCT6791(), fans: 6}, true
		}
	case 0xC9:
		switch revision {
		case 0x11:
			return superIOChip{Name: "Nuvoton NCT6792D", temps: tempsNCT6791(), fans: 6}, true
		case 0x13:
			return superIOChip{Name: "Nuvoton NCT6792D-A", temps: tempsNCT6791(), fans: 6}, true
		}
	case 0xD1:
		if revision == 0x21 {
			return superIOChip{Name: "Nuvoton NCT6793D", temps: tempsNCT6791(), fans: 6}, true
		}
	case 0xD3:
		if revision == 0x52 {
			return superIOChip{Name: "Nuvoton NCT6795D", temps: tempsNCT6791(), fans: 6}, true
		}
	case 0xD4:
		switch revision {
		case 0x23:
			return superIOChip{Name: "Nuvoton NCT6796D", temps: tempsNCT6796(), fans: 6}, true
		case 0x2A:
			return superIOChip{Name: "Nuvoton NCT6796D-R", temps: tempsNCT6796(), fans: 7}, true
		case 0x51:
			return superIOChip{Name: "Nuvoton NCT6797D", temps: tempsNCT6796(), fans: 7}, true
		case 0x2B:
			return superIOChip{Name: "Nuvoton NCT6798D", temps: tempsNCT6798(), fans: 7}, true
		}
	case 0xD8:
		if revision == 0x02 {
			return superIOChip{Name: "Nuvoton NCT6799D", temps: tempsNCT6798(), fans: 7}, true
		}
	}
	return superIOChip{}, false
}

// chipIDString renders a chip identity the way a datasheet and a field report
// spell it.
func chipIDString(id, revision byte) string {
	return fmt.Sprintf("0x%02X/0x%02X", id, revision)
}

// invalidMonitorBase reports whether a base address the configuration window
// handed out cannot be a monitor window: below the fixed I/O range, or not on
// the 8-byte boundary the window needs. A chip whose logical device is
// disabled answers 0x0000 or 0xFFFF, both of which this rejects.
func invalidMonitorBase(address uint16) bool {
	return address < 0x100 || address&0xF007 != 0
}

// hwmWindow is the chip's hardware-monitor register window.
type hwmWindow struct {
	io   portIO
	base uint16
}

// readByte reads one monitor register. address is bank<<8 | register: the
// bank is selected by writing it through index 0x4E, then the register byte
// goes in the index port and the value comes out of the data port.
func (w hwmWindow) readByte(address uint16) (byte, error) {
	if err := w.selectRegister(address); err != nil {
		return 0, err
	}
	v, err := w.io.inb(w.base + hwmDataOffset)
	if err != nil {
		return 0, fmt.Errorf("read monitor register 0x%03X: %w", address, err)
	}
	return v, nil
}

// writeByte writes one monitor register.
func (w hwmWindow) writeByte(address uint16, value byte) error {
	if err := w.selectRegister(address); err != nil {
		return err
	}
	if err := w.io.outb(w.base+hwmDataOffset, value); err != nil {
		return fmt.Errorf("write monitor register 0x%03X: %w", address, err)
	}
	return nil
}

func (w hwmWindow) selectRegister(address uint16) error {
	for _, step := range [...]struct {
		port  uint16
		value byte
	}{
		{w.base + hwmAddressOffset, hwmBankSelect},
		{w.base + hwmDataOffset, byte(address >> 8)},
		{w.base + hwmAddressOffset, byte(address)},
	} {
		if err := w.io.outb(step.port, step.value); err != nil {
			return fmt.Errorf("select monitor register 0x%03X: %w", address, err)
		}
	}
	return nil
}

// vendorID reads Nuvoton's identifier back out of the monitor window. It is
// the proof the window is open and answering: a locked or absent window
// returns 0xFF from every register, which is not this value.
func (w hwmWindow) vendorID() (uint16, error) {
	high, err := w.readByte(hwmVendorIDHigh)
	if err != nil {
		return 0, err
	}
	low, err := w.readByte(hwmVendorIDLow)
	if err != nil {
		return 0, err
	}
	return uint16(high)<<8 | uint16(low), nil
}

// readFunc is one monitor register read, so the decoders below are exercised
// against captured bytes as easily as against a live chip.
type readFunc func(address uint16) (byte, error)

// decodeTemperatures reads every input of a chip and returns the ones that
// produced a usable reading, in the chip's own input order.
//
// The two passes mirror how the chip presents its sensors. A switchable
// register is read first and asked which input it is currently carrying; the
// first register to claim an input wins, because the later ones that report
// the same input do so without the half-degree bit. Inputs that nothing
// claimed are then retried against their whole-degree mirror register, which
// is where a firmware that did not assign a switchable slot still publishes
// them.
//
// An input that is not wired up reads as noise outside -55..125 °C and is
// dropped: a board with three thermistors fitted reports three temperatures,
// never ten, and never a zero standing in for a sensor that is not there.
func decodeTemperatures(chip superIOChip, read readFunc) ([]hostsensors.BoardTemp, error) {
	values := make(map[tempSource]float64, len(chip.temps))
	claimed := make(map[tempSource]bool, len(chip.temps))

	for _, in := range chip.temps {
		if in.reg == 0 {
			continue
		}
		raw, err := read(in.reg)
		if err != nil {
			return nil, err
		}
		// Whole degrees are two's complement; the value is carried at half
		// a degree's resolution, so it is doubled before the fractional bit
		// is folded in.
		value := int(int8(raw)) << 1
		if in.halfBit > 0 {
			half, err := read(in.halfReg)
			if err != nil {
				return nil, err
			}
			value |= int(half>>uint(in.halfBit)) & 0x1
		}
		source := in.source
		if in.sourceReg != 0 {
			s, err := read(in.sourceReg)
			if err != nil {
				return nil, err
			}
			source = tempSource(s & 0x1F)
		}
		if claimed[source] {
			continue
		}
		celsius := 0.5 * float64(value)
		if !usableCelsius(celsius) {
			continue
		}
		claimed[source] = true
		values[source] = celsius
	}

	for _, in := range chip.temps {
		if in.altReg == 0 || claimed[in.source] {
			continue
		}
		raw, err := read(in.altReg)
		if err != nil {
			return nil, err
		}
		// The mirror registers carry whole degrees only.
		celsius := float64(int8(raw))
		if !usableCelsius(celsius) {
			continue
		}
		claimed[in.source] = true
		values[in.source] = celsius
	}

	out := make([]hostsensors.BoardTemp, 0, len(values))
	emitted := make(map[tempSource]bool, len(values))
	for _, in := range chip.temps {
		celsius, ok := values[in.source]
		if !ok || emitted[in.source] {
			continue
		}
		emitted[in.source] = true
		name, known := tempSourceNames[in.source]
		if !known {
			name = tempSourceName{
				label: fmt.Sprintf("Input %d", in.source),
				input: fmt.Sprintf("SOURCE_%d", in.source),
				role:  hostsensors.RoleAux,
			}
		}
		out = append(out, hostsensors.BoardTemp{
			Label:   name.label,
			Input:   name.input,
			Role:    name.role,
			Celsius: celsius,
		})
	}
	return out, nil
}

// usableCelsius reports whether a decoded reading is a measurement rather
// than an unconnected input's noise.
func usableCelsius(celsius float64) bool {
	return celsius > 0 && celsius <= tempMaxCelsius
}

// decodeFanRPM turns one tachometer channel's 13-bit count into revolutions a
// minute. A count at full scale is a header that is not turning, which is a
// real 0 RPM; a count below what the conversion can express is not a reading
// at all and ok is false.
func decodeFanRPM(high, low byte) (rpm uint32, ok bool) {
	count := int(high)<<5 | int(low&0x1F)
	switch {
	case count >= fanMaxCount:
		return 0, true
	case count < fanMinCount:
		return 0, false
	default:
		return uint32(math.Round(fanCountToRPM / float64(count))), true
	}
}

// decodeFans reads every tachometer channel the chip has.
func decodeFans(chip superIOChip, read readFunc) ([]hostsensors.BoardFan, error) {
	count := chip.fans
	if count > len(fanCountRegisters) {
		count = len(fanCountRegisters)
	}
	out := make([]hostsensors.BoardFan, 0, count)
	for i := 0; i < count; i++ {
		high, err := read(fanCountRegisters[i])
		if err != nil {
			return nil, err
		}
		low, err := read(fanCountRegisters[i] + 1)
		if err != nil {
			return nil, err
		}
		rpm, ok := decodeFanRPM(high, low)
		if !ok {
			continue
		}
		out = append(out, hostsensors.BoardFan{
			Index: i,
			Label: fmt.Sprintf("Fan %d", i+1),
			RPM:   rpm,
		})
	}
	return out, nil
}

// decodeVcore reads the CPU core voltage from the chip's first analogue
// input. A zero reading means the input is not wired and is reported as zero,
// which the wire format drops.
func decodeVcore(read readFunc) (float64, error) {
	raw, err := read(vcoreRegister)
	if err != nil {
		return 0, err
	}
	return voltsPerStep * float64(raw), nil
}
