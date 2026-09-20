// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// Intel digital thermal sensor decoding (Intel SDM vol. 3B, "Thermal
// Monitoring and Protection"). Platform-neutral so the bit maths is unit
// tested everywhere; the register reads themselves are in intel_windows.go.

const (
	// msrIA32ThermStatus is the per-core thermal status register.
	msrIA32ThermStatus = 0x19C
	// msrIA32TemperatureTarget carries TjMax, the junction temperature the
	// digital readouts are relative to.
	msrIA32TemperatureTarget = 0x1A2
	// msrIA32PackageThermStatus is the package thermal status register: the
	// hottest reading across the package, valid from any core.
	msrIA32PackageThermStatus = 0x1B1

	sourceIntelMSR = "intel-msr"
)

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
