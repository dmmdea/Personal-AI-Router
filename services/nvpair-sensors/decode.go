// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"math"
	"time"
)

// Intel digital thermal sensor and RAPL energy decoding (Intel SDM vol. 3B,
// "Thermal Monitoring and Protection" and "Platform Specific Power Management
// Support"). Platform-neutral so the bit maths is unit tested everywhere; the
// register reads themselves are in intel_windows.go.

const (
	// msrIA32ThermStatus is the per-core thermal status register.
	msrIA32ThermStatus = 0x19C
	// msrIA32TemperatureTarget carries TjMax, the junction temperature the
	// digital readouts are relative to.
	msrIA32TemperatureTarget = 0x1A2
	// msrIA32PackageThermStatus is the package thermal status register: the
	// hottest reading across the package, valid from any core.
	msrIA32PackageThermStatus = 0x1B1
	// msrRAPLPowerUnit carries the scaling factors for every RAPL counter on
	// the part; bits 12:8 are the energy status unit this file needs.
	msrRAPLPowerUnit = 0x606
	// msrPkgEnergyStatus is the package energy counter: a 32-bit total of
	// energy units consumed, which wraps and is never reset.
	msrPkgEnergyStatus = 0x611

	sourceIntelMSR = "intel-msr"
)

// maxPlausiblePackageWatts bounds a derived figure. A package that appears to
// have drawn more than this between two samples did not — the counter was
// re-based under us (a resume from sleep, a driver reload) and the delta is a
// distance around the wrap, not energy. Such a sample publishes nothing, which
// is the same answer the first sample after a start gives.
const maxPlausiblePackageWatts = 1000

// Both RAPL registers this file reads are package-scope, so like the thermal
// package register they read the same from every core and the module needs no
// thread affinity — which matters because the signed IntelMSR module exposes
// no affinity control to its callers.

// decodeEnergyUnit extracts the energy status unit from MSR_RAPL_POWER_UNIT
// (bits 12:8) and returns the joules one counter tick represents: energy is
// counted in 1/2^ESU joules, which is 61.0 microjoules on the ESU=14 parts and
// 15.3 on the ESU=16 ones.
//
// ok is false for an ESU of zero, which would claim a whole joule per tick —
// no part reports that, so it means the register was not the one we think.
func decodeEnergyUnit(v uint64) (joulesPerTick float64, ok bool) {
	esu := uint32(v>>8) & 0x1F
	if esu == 0 {
		return 0, false
	}
	return 1 / math.Pow(2, float64(esu)), true
}

// pkgEnergyCounter extracts the running total from MSR_PKG_ENERGY_STATUS. The
// counter is bits 31:0; bits 63:32 are reserved and must not be folded in.
func pkgEnergyCounter(v uint64) uint32 { return uint32(v) }

// energyDelta is the number of counter ticks consumed between two reads.
//
// The counter only ever increments and wraps at 2^32, and unsigned subtraction
// wraps identically, so this single expression is already correct across a
// rollover — cur < prev yields the distance the counter travelled, not a
// negative. Naming it makes that deliberate rather than accidental.
func energyDelta(prev, cur uint32) uint32 { return cur - prev }

// packageWatts converts a counter delta into average power over the interval
// it was measured across, rounded to whole watts.
//
// ok is false whenever the result cannot be a real reading: a non-positive
// interval, an unresolved energy unit, a delta of zero (a running package
// always consumes something, so zero means the counter did not advance and the
// read is not what we think), or a figure past maxPlausiblePackageWatts.
func packageWatts(ticks uint32, joulesPerTick float64, elapsed time.Duration) (float64, bool) {
	seconds := elapsed.Seconds()
	if seconds <= 0 || joulesPerTick <= 0 || ticks == 0 {
		return 0, false
	}
	w := math.Round(float64(ticks) * joulesPerTick / seconds)
	if w <= 0 || w > maxPlausiblePackageWatts {
		return 0, false
	}
	return w, true
}

// decodeTjMax extracts the temperature target from IA32_TEMPERATURE_TARGET
// (bits 23:16, whole degrees Celsius). ok is false when the CPU reports none.
func decodeTjMax(v uint64) (celsius uint32, ok bool) {
	t := uint32(v>>16) & 0xFF
	return t, t != 0
}

// decodeThermStatus splits a thermal status register (IA32_THERM_STATUS or
// IA32_PACKAGE_THERM_STATUS) into its digital readout — bits 22:16, degrees
// below TjMax — and the reading-valid bit 31.
func decodeThermStatus(v uint64) (deltaBelowTjMax uint32, valid bool) {
	return uint32(v>>16) & 0x7F, v&(1<<31) != 0
}

// packageCelsius converts a digital readout into an absolute temperature.
// ok is false when the readout exceeds TjMax, which no real sensor produces.
func packageCelsius(tjMax, deltaBelowTjMax uint32) (celsius uint32, ok bool) {
	if deltaBelowTjMax > tjMax {
		return 0, false
	}
	return tjMax - deltaBelowTjMax, true
}
