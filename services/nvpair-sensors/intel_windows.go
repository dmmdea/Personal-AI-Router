// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"golang.org/x/sys/windows/registry"
)

// intelMSRModule is the signed IntelMSR module from PawnIO.Modules (see
// modules/NOTICE.md for version, checksum and license). The driver verifies
// its signature at load; the module itself refuses to load on a non-Intel or
// non-x64 CPU.
//
//go:embed modules/IntelMSR.bin
var intelMSRModule []byte

const cpuVendorIntel = "GenuineIntel"

// intelPackageSensor reads the package temperature and the package power
// draw: TjMax and the RAPL energy unit once at open, then
// IA32_PACKAGE_THERM_STATUS and MSR_PKG_ENERGY_STATUS per sample. All four
// registers are package-scope and read the same from every core, so no thread
// affinity is needed — which is just as well, because the signed IntelMSR
// module exposes none.
//
// Power is a derivative: MSR_PKG_ENERGY_STATUS is a running total, so the
// sample state below holds the previous read and the first sample after an
// open produces no figure at all rather than a fabricated one.
type intelPackageSensor struct {
	dev    *pawnIO
	target uint32 // TjMax
	// joulesPerTick is the MSR_RAPL_POWER_UNIT energy scale, zero on a part
	// whose unit could not be resolved. Zero disables the power read and
	// nothing else: the temperature is the reading every consumer depends on
	// and must not be lost over a register that is new here.
	joulesPerTick float64

	prevEnergy uint32
	prevAt     time.Time
	havePrev   bool
}

// openIntelPackageSensor opens PawnIO, loads the Intel module and resolves
// TjMax plus the RAPL energy unit. Every failure names the reason so the
// helper can publish it — except a missing energy unit, which is reported as
// a note and leaves the temperature sensor fully usable.
func openIntelPackageSensor() (*intelPackageSensor, error) {
	if v := cpuVendor(); v != "" && v != cpuVendorIntel {
		return nil, fmt.Errorf("CPU vendor %q: this build reads the Intel package sensor only", v)
	}
	dev, err := openPawnIO()
	if err != nil {
		return nil, err
	}
	if err := dev.load(intelMSRModule); err != nil {
		dev.close()
		return nil, fmt.Errorf("load IntelMSR module: %w", err)
	}
	raw, err := dev.readMSR(msrIA32TemperatureTarget)
	if err != nil {
		dev.close()
		return nil, err
	}
	tjMax, ok := decodeTjMax(raw)
	if !ok {
		dev.close()
		return nil, errors.New("IA32_TEMPERATURE_TARGET reports no TjMax on this CPU")
	}
	s := &intelPackageSensor{dev: dev, target: tjMax}
	// MSR_RAPL_POWER_UNIT is on the module's read allow list beside the
	// thermal registers, but a part can still refuse it (a virtualized CPU
	// that traps RAPL, a pre-Sandy-Bridge core). That is a missing figure,
	// never a failed open: this helper exists for the temperature.
	if unit, err := dev.readMSR(msrRAPLPowerUnit); err != nil {
		slog.Info("no CPU package power source; cpu.package_watts will be omitted", "err", err)
	} else if joules, ok := decodeEnergyUnit(unit); !ok {
		slog.Info("no CPU package power source; cpu.package_watts will be omitted",
			"reason", "MSR_RAPL_POWER_UNIT reports no energy unit on this CPU")
	} else {
		s.joulesPerTick = joules
	}
	return s, nil
}

// openPackageSensor is the sampler's open hook: the Intel sensor as the
// packageSensor interface, with a failed open returning a nil interface
// rather than a typed nil.
func openPackageSensor() (packageSensor, error) {
	s, err := openIntelPackageSensor()
	if err != nil {
		return nil, err
	}
	return s, nil
}

// read returns the current package temperature in whole degrees.
func (s *intelPackageSensor) read() (uint32, error) {
	raw, err := s.dev.readMSR(msrIA32PackageThermStatus)
	if err != nil {
		return 0, err
	}
	delta, valid := decodeThermStatus(raw)
	if !valid {
		return 0, errors.New("IA32_PACKAGE_THERM_STATUS: reading not valid")
	}
	c, ok := packageCelsius(s.target, delta)
	if !ok {
		return 0, fmt.Errorf("IA32_PACKAGE_THERM_STATUS: readout %d exceeds TjMax %d", delta, s.target)
	}
	return c, nil
}

// power returns the average package power in whole watts over the interval
// since the previous call, and records this sample as the next baseline.
//
// ok is false whenever there is no honest figure: no energy unit, a failed
// register read, the first sample after an open, or a delta that can only be
// a counter re-base. Each of those drops the baseline, so a transient failure
// costs one sample rather than freezing a stale number into every report.
//
// It never returns an error. A power read that failed must not look like a
// sensor fault to the sampler: that would trip the reopen-after-three-failures
// path and cost the host its temperature.
func (s *intelPackageSensor) power(now time.Time) (float64, bool) {
	if s.joulesPerTick <= 0 {
		return 0, false
	}
	raw, err := s.dev.readMSR(msrPkgEnergyStatus)
	if err != nil {
		s.havePrev = false
		return 0, false
	}
	cur := pkgEnergyCounter(raw)
	prev, prevAt, havePrev := s.prevEnergy, s.prevAt, s.havePrev
	s.prevEnergy, s.prevAt, s.havePrev = cur, now, true
	if !havePrev {
		return 0, false
	}
	watts, ok := packageWatts(energyDelta(prev, cur), s.joulesPerTick, now.Sub(prevAt))
	if !ok {
		s.havePrev = false
		return 0, false
	}
	return watts, true
}

func (s *intelPackageSensor) tjMax() uint32 { return s.target }

func (s *intelPackageSensor) close() {
	if s != nil {
		s.dev.close()
	}
}

// cpuVendor reads the boot processor's vendor string from the registry the
// kernel populates at boot. Empty when unreadable (the module's own check
// still applies).
func cpuVendor() string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `HARDWARE\DESCRIPTION\System\CentralProcessor\0`, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	v, _, err := k.GetStringValue("VendorIdentifier")
	if err != nil {
		return ""
	}
	return v
}
