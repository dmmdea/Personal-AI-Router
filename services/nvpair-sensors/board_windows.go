// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	_ "embed"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"nvpair-shared/hostsensors"
)

// Motherboard sensors on Windows, through the signed PawnIO LpcIO module.
//
// Why a driver is involved at all: the Super I/O chip answers on two x86 I/O
// ports, and a user-mode process cannot touch those on any supported Windows
// version. The same PawnIO driver the CPU package sensor already uses carries
// a second signed module, LpcIO, whose whole job is this chip: it opens the
// configuration window, discovers which port ranges the chip actually claims,
// and refuses every port outside them. So the privilege this grants is bounded
// by the module, not by this service (see modules/NOTICE.md).
//
// What the board publishes and what it does not. This board's own management
// controllers — ASUS's TPU (TurboV Processing Unit) and EPU (Energy Processing
// Unit), marketed together as Dual Intelligent Processors 5 — expose no status
// or telemetry interface: the board's root\wmi namespace carries ASUSHW (raw
// SMBus transfers), ASUSManagement (display and EC event control) and
// AsusWpbtWmi, and the sensor_get_* methods the Ryzen-era boards answer do not
// exist here. Presence plus the sensor set those controllers act on is
// therefore the honest readout, and it is what this file produces.

// lpcIOModule is the signed LpcIO module from PawnIO.Modules (see
// modules/NOTICE.md for version, checksum and license). The driver verifies
// its signature at load.
//
//go:embed modules/LpcIO.bin
var lpcIOModule []byte

// isaBusMutexName is the system-wide gate every tool that pokes the Super I/O
// chip through the LPC bus takes before it touches the index/data ports:
// LibreHardwareMonitor, HWiNFO and the vendor dashboards all name this
// mutant, and PawnIO's LpcIO module documents it as the caller's
// responsibility. Two readers interleaving a bank select and a register read
// would each get the other's register.
const isaBusMutexName = `Global\Access_ISABUS.HTP.Method`

// isaBusWaitTimeout bounds the wait for that gate. A sample that cannot get
// it inside this window is skipped rather than forced: the holder is another
// tool mid-transaction, and a few seconds later there is another sample.
const isaBusWaitTimeout = 200 * time.Millisecond

// mutexModifyState is MUTEX_MODIFY_STATE, the right needed to release a
// mutant; SYNCHRONIZE is the right needed to wait on one.
const mutexModifyState = 0x0001

// baseAddressSettleDelay separates the two reads of the base-address
// register. The value has to be identical both times: a chip that is not
// really there, or a window that is still coming up, answers differently.
const baseAddressSettleDelay = time.Millisecond

// smbiosKey is where the kernel publishes the SMBIOS type-2 (baseboard)
// strings at boot. Reading them here rather than over WMI keeps the helper
// free of a COM dependency and works identically under LocalSystem.
const smbiosKey = `HARDWARE\DESCRIPTION\System\BIOS`

var (
	errNoSuperIO      = errors.New("no supported Super I/O chip answered at 0x2E or 0x4E")
	errMonitorLocked  = errors.New("the Super I/O monitor window did not report Nuvoton's vendor id: it is locked or absent")
	errISABusBusy     = errors.New("another tool holds the ISA bus gate; skipping this board sample")
	errNoBoardSupport = errors.New("this build reads Nuvoton NCT67xx Super I/O chips only")
)

// isaBusGate is the process's handle to the shared ISA bus mutant.
//
// A host where the object cannot be created or opened still gets readings:
// the gate is cooperative, every tool that takes it is optional, and refusing
// to read because a coordination primitive is unavailable would trade a real
// measurement for a theoretical conflict. That case is named in the error the
// sensor reports at open, so it shows up rather than passing silently.
type isaBusGate struct {
	handle windows.Handle
}

// openISABusGate creates the mutant, or opens the one another tool already
// made. It is created with a DACL that grants every caller full control, the
// same terms LibreHardwareMonitor creates it under — an unprivileged tool has
// to be able to take it too, or the gate arbitrates nothing.
func openISABusGate() (*isaBusGate, error) {
	name, err := windows.UTF16PtrFromString(isaBusMutexName)
	if err != nil {
		return nil, err
	}
	sd, err := windows.SecurityDescriptorFromString("D:(A;;0x1F0001;;;WD)")
	if err != nil {
		return nil, fmt.Errorf("build the ISA bus gate's security descriptor: %w", err)
	}
	sa := windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(sa))
	h, err := windows.CreateMutex(&sa, false, name)
	if err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		// Another tool owns the object with a DACL that will not let this
		// one recreate it; opening it for wait and release is enough.
		h, err = windows.OpenMutex(windows.SYNCHRONIZE|mutexModifyState, false, name)
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", isaBusMutexName, err)
		}
	}
	return &isaBusGate{handle: h}, nil
}

// acquire waits for the gate. ok is false when the wait timed out.
//
// It pins the calling goroutine to its OS thread for as long as the gate is
// held, and release unpins it. That is not an optimisation: a Win32 mutant is
// owned by the THREAD that waited on it and only that thread may release it,
// while a goroutine is free to move between OS threads whenever it blocks. A
// release from the wrong thread fails with ERROR_NOT_OWNER and the gate stays
// held for the life of the process — locking out not just this sampler but
// every other tool on the machine that reads the same chip. Measured on the
// target board before this pin existed: the helper wedged its own board
// sampler, and the vendor's fan-control service with it, four seconds after
// the service started.
func (g *isaBusGate) acquire() (ok bool, err error) {
	if g == nil || g.handle == 0 {
		return true, nil
	}
	runtime.LockOSThread()
	state, waitErr := windows.WaitForSingleObject(g.handle, uint32(isaBusWaitTimeout.Milliseconds()))
	switch {
	case waitErr != nil:
		runtime.UnlockOSThread()
		return false, fmt.Errorf("wait for %s: %w", isaBusMutexName, waitErr)
	case state == uint32(windows.WAIT_TIMEOUT):
		runtime.UnlockOSThread()
		return false, nil
	case state == windows.WAIT_OBJECT_0, state == uint32(windows.WAIT_ABANDONED):
		// WAIT_ABANDONED means the previous owner died holding it. The gate
		// is ours either way, and the chip has no state a half-finished
		// transaction could have left behind: every read re-selects its own
		// bank and register.
		return true, nil
	default:
		runtime.UnlockOSThread()
		return false, fmt.Errorf("wait for %s: unexpected state 0x%08X", isaBusMutexName, state)
	}
}

// release hands the gate back and unpins the thread acquire pinned.
//
// A failed release is reported rather than swallowed. It means the gate is now
// wedged, which is a machine-wide fault and not a private one, so the caller
// turns it into a visible error instead of quietly sampling on.
func (g *isaBusGate) release() error {
	if g == nil || g.handle == 0 {
		return nil
	}
	err := windows.ReleaseMutex(g.handle)
	runtime.UnlockOSThread()
	if err != nil {
		return fmt.Errorf("release %s: %w", isaBusMutexName, err)
	}
	return nil
}

func (g *isaBusGate) close() {
	if g == nil || g.handle == 0 {
		return
	}
	windows.CloseHandle(g.handle)
	g.handle = 0
}

// lpcPortIO is the portIO implementation over the LpcIO module. Every read
// and every write is one driver round trip the module bounds to the ports it
// discovered for this chip.
type lpcPortIO struct{ dev *pawnIO }

func (p lpcPortIO) inb(port uint16) (byte, error) {
	out, err := p.dev.call("ioctl_pio_inb", []uint64{uint64(port)}, 1)
	if err != nil {
		return 0, fmt.Errorf("read port 0x%04X: %w", port, err)
	}
	if len(out) < 1 {
		return 0, fmt.Errorf("read port 0x%04X: the module returned no value", port)
	}
	return byte(out[0]), nil
}

func (p lpcPortIO) outb(port uint16, value byte) error {
	if _, err := p.dev.call("ioctl_pio_outb", []uint64{uint64(port), uint64(value)}, 0); err != nil {
		return fmt.Errorf("write port 0x%04X: %w", port, err)
	}
	return nil
}

// superIOSensor is one open Super I/O chip: the loaded module, the shared
// bus gate, the decoded chip identity and its monitor window.
type superIOSensor struct {
	dev    *pawnIO
	gate   *isaBusGate
	io     lpcPortIO
	window hwmWindow

	chip       superIOChip
	id         byte
	revision   byte
	configPort uint16

	vendor  string
	product string

	// note carries a non-fatal condition the caller should log once, empty
	// when the sensor opened cleanly.
	note string
}

// configSlots are the two windows a Super I/O chip can answer on, in the
// order they are probed: the primary one first, because that is where a
// single-chip board puts it.
var configSlots = [...]struct {
	slot int
	port uint16
}{
	{0, configPortPrimary},
	{1, configPortSecondary},
}

// openBoardSensor loads the LpcIO module, finds the chip, and leaves the
// monitor window open and answering. Every failure names its reason so the
// helper can publish it instead of an absent section with no explanation.
func openBoardSensor() (boardSensor, error) {
	dev, err := openPawnIO()
	if err != nil {
		return nil, err
	}
	if err := dev.load(lpcIOModule); err != nil {
		dev.close()
		return nil, fmt.Errorf("load LpcIO module: %w", err)
	}
	s := &superIOSensor{dev: dev, io: lpcPortIO{dev: dev}}
	s.vendor, s.product = baseboardIdentity()

	gate, gateErr := openISABusGate()
	s.gate = gate

	if err := s.detect(); err != nil {
		s.close()
		if gateErr != nil {
			return nil, fmt.Errorf("%w (the ISA bus gate was also unavailable: %v)", err, gateErr)
		}
		return nil, err
	}
	if gateErr != nil {
		// Detection worked without arbitration. Reading anyway is the right
		// trade — the gate is cooperative and every tool that takes it is
		// optional — but it is not silent.
		s.note = "reading the Super I/O chip without the shared ISA bus gate: " + gateErr.Error()
	}
	return s, nil
}

// detect walks both configuration slots and stops at the first supported
// chip, leaving its monitor window unlocked and verified.
func (s *superIOSensor) detect() (err error) {
	held, err := s.gate.acquire()
	if err != nil {
		return err
	}
	if !held {
		return errISABusBusy
	}
	defer func() {
		if relErr := s.gate.release(); relErr != nil && err == nil {
			err = relErr
		}
	}()

	var lastID, lastRevision byte
	for _, c := range configSlots {
		ok, id, revision, err := s.trySlot(c.slot, c.port)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		if id != 0x00 && id != 0xFF {
			lastID, lastRevision = id, revision
		}
	}
	if lastID != 0 {
		return fmt.Errorf("%w: the chip that answered identifies as %s", errNoBoardSupport, chipIDString(lastID, lastRevision))
	}
	return errNoSuperIO
}

// trySlot opens one configuration window, identifies the chip behind it and,
// when it is one this build decodes, resolves and unlocks its monitor
// window. The window is always closed again before returning.
func (s *superIOSensor) trySlot(slot int, port uint16) (found bool, id, revision byte, err error) {
	if err := s.selectSlot(slot); err != nil {
		return false, 0, 0, err
	}
	s.configPort = port
	if err := s.enterConfig(); err != nil {
		return false, 0, 0, err
	}
	// From here every path closes the window, including the error ones: a
	// configuration window left open is a foot-gun for every other tool on
	// the box.
	defer func() {
		if exitErr := s.exitConfig(); exitErr != nil && err == nil {
			err = exitErr
		}
	}()

	if id, err = s.cfgInb(cfgChipID); err != nil {
		return false, 0, 0, err
	}
	if revision, err = s.cfgInb(cfgChipRevision); err != nil {
		return false, id, 0, err
	}
	chip, ok := superIOChipFor(id, revision)
	if !ok {
		return false, id, revision, nil
	}

	// The module's port allow list starts empty; this is what fills it with
	// the ranges the chip's logical devices claim, and it has to happen
	// while the window is open and the chip identity reads back.
	if err = s.findBars(); err != nil {
		return false, id, revision, err
	}
	if err = s.cfgOutb(cfgDeviceSelect, hwmLogicalDevice); err != nil {
		return false, id, revision, err
	}
	base, err := s.cfgInw(cfgBaseAddress)
	if err != nil {
		return false, id, revision, err
	}
	time.Sleep(baseAddressSettleDelay)
	verify, err := s.cfgInw(cfgBaseAddress)
	if err != nil {
		return false, id, revision, err
	}
	if base != verify {
		return false, id, revision, fmt.Errorf("%s reported base address 0x%04X then 0x%04X: the window is not stable", chip.Name, base, verify)
	}
	if invalidMonitorBase(base) {
		return false, id, revision, fmt.Errorf("%s reported monitor base address 0x%04X, which is not a usable window", chip.Name, base)
	}
	if err = s.disableIOSpaceLock(); err != nil {
		return false, id, revision, err
	}

	s.chip, s.id, s.revision = chip, id, revision
	s.window = hwmWindow{io: s.io, base: base}
	vendor, err := s.window.vendorID()
	if err != nil {
		return false, id, revision, err
	}
	if vendor != hwmVendorNuvoton {
		return false, id, revision, fmt.Errorf("%w (read 0x%04X)", errMonitorLocked, vendor)
	}
	return true, id, revision, nil
}

// read takes one full sample of the board.
func (s *superIOSensor) read() (reading hostsensors.BoardReading, err error) {
	held, err := s.gate.acquire()
	if err != nil {
		return hostsensors.BoardReading{}, err
	}
	if !held {
		return hostsensors.BoardReading{}, errISABusBusy
	}
	defer func() {
		if relErr := s.gate.release(); relErr != nil && err == nil {
			reading, err = hostsensors.BoardReading{}, relErr
		}
	}()

	if err := s.ensureMonitorOpen(); err != nil {
		return hostsensors.BoardReading{}, err
	}
	temps, err := decodeTemperatures(s.chip, s.window.readByte)
	if err != nil {
		return hostsensors.BoardReading{}, err
	}
	fans, err := decodeFans(s.chip, s.window.readByte)
	if err != nil {
		return hostsensors.BoardReading{}, err
	}
	vcore, err := decodeVcore(s.window.readByte)
	if err != nil {
		return hostsensors.BoardReading{}, err
	}
	return hostsensors.BoardReading{
		Chip:         s.chip.Name,
		ChipID:       chipIDString(s.id, s.revision),
		Vendor:       s.vendor,
		Product:      s.product,
		Temperatures: temps,
		Fans:         fans,
		VcoreVolts:   vcore,
		Source:       sourceSuperIO,
	}, nil
}

// ensureMonitorOpen re-checks the window before a sample and clears the lock
// again if something re-armed it. The vendor's own utility re-locks the
// window when it starts, and every register then reads 0xFF — which decodes
// as a plausible-looking pile of temperatures, so this check is what keeps
// that from being published as data.
func (s *superIOSensor) ensureMonitorOpen() error {
	vendor, err := s.window.vendorID()
	if err != nil {
		return err
	}
	if vendor == hwmVendorNuvoton {
		return nil
	}
	if err := s.enterConfig(); err != nil {
		return err
	}
	unlockErr := s.disableIOSpaceLock()
	exitErr := s.exitConfig()
	if unlockErr != nil {
		return unlockErr
	}
	if exitErr != nil {
		return exitErr
	}
	if vendor, err = s.window.vendorID(); err != nil {
		return err
	}
	if vendor != hwmVendorNuvoton {
		return fmt.Errorf("%w (read 0x%04X)", errMonitorLocked, vendor)
	}
	return nil
}

func (s *superIOSensor) close() {
	if s == nil {
		return
	}
	s.gate.close()
	s.dev.close()
}

// openNote is the non-fatal condition the sampler logs once at open.
func (s *superIOSensor) openNote() string { return s.note }

// ---------------------------------------------------------------------------
// Configuration-window primitives
// ---------------------------------------------------------------------------

// selectSlot points the module at one of the two configuration windows. This
// also resets the module's port allow list, so findBars has to run again.
func (s *superIOSensor) selectSlot(slot int) error {
	if _, err := s.dev.call("ioctl_select_slot", []uint64{uint64(slot)}, 0); err != nil {
		return fmt.Errorf("select Super I/O slot %d: %w", slot, err)
	}
	return nil
}

// findBars has the module walk every logical device and record the port
// ranges they claim, which is what its allow list is built from.
func (s *superIOSensor) findBars() error {
	if _, err := s.dev.call("ioctl_find_bars", nil, 0); err != nil {
		return fmt.Errorf("discover the chip's port ranges: %w", err)
	}
	return nil
}

// enterConfig opens the configuration window: the magic byte, twice.
func (s *superIOSensor) enterConfig() error {
	for i := 0; i < 2; i++ {
		if err := s.io.outb(s.configPort, cfgEnter); err != nil {
			return fmt.Errorf("open the Super I/O configuration window at 0x%04X: %w", s.configPort, err)
		}
	}
	return nil
}

// exitConfig closes it again.
func (s *superIOSensor) exitConfig() error {
	if err := s.io.outb(s.configPort, cfgExit); err != nil {
		return fmt.Errorf("close the Super I/O configuration window at 0x%04X: %w", s.configPort, err)
	}
	return nil
}

func (s *superIOSensor) cfgInb(reg byte) (byte, error) {
	out, err := s.dev.call("ioctl_superio_inb", []uint64{uint64(reg)}, 1)
	if err != nil {
		return 0, fmt.Errorf("read configuration register 0x%02X: %w", reg, err)
	}
	if len(out) < 1 {
		return 0, fmt.Errorf("read configuration register 0x%02X: the module returned no value", reg)
	}
	return byte(out[0]), nil
}

func (s *superIOSensor) cfgInw(reg byte) (uint16, error) {
	out, err := s.dev.call("ioctl_superio_inw", []uint64{uint64(reg)}, 1)
	if err != nil {
		return 0, fmt.Errorf("read configuration register pair 0x%02X: %w", reg, err)
	}
	if len(out) < 1 {
		return 0, fmt.Errorf("read configuration register pair 0x%02X: the module returned no value", reg)
	}
	return uint16(out[0]), nil
}

func (s *superIOSensor) cfgOutb(reg, value byte) error {
	if _, err := s.dev.call("ioctl_superio_outb", []uint64{uint64(reg), uint64(value)}, 0); err != nil {
		return fmt.Errorf("write configuration register 0x%02X: %w", reg, err)
	}
	return nil
}

// disableIOSpaceLock clears the bit that makes the monitor window read back
// as 0xFF. Must be called with the configuration window open. It is the only
// write this service makes to the chip, and it is a read-modify-write of one
// documented bit — no other bit of that register is touched.
func (s *superIOSensor) disableIOSpaceLock() error {
	options, err := s.cfgInb(cfgIOSpaceLock)
	if err != nil {
		return err
	}
	if options&cfgIOSpaceLockBit == 0 {
		return nil
	}
	return s.cfgOutb(cfgIOSpaceLock, options&^cfgIOSpaceLockBit)
}

// ---------------------------------------------------------------------------
// Board identity
// ---------------------------------------------------------------------------

// baseboardIdentity reads the SMBIOS type-2 manufacturer and product the
// kernel publishes at boot. Either can be empty on a board whose firmware
// leaves the field blank; a reader gates on what it got rather than assuming.
func baseboardIdentity() (vendor, product string) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, smbiosKey, registry.QUERY_VALUE)
	if err != nil {
		return "", ""
	}
	defer k.Close()
	read := func(name string) string {
		v, _, err := k.GetStringValue(name)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(v)
	}
	return read("BaseBoardManufacturer"), read("BaseBoardProduct")
}
