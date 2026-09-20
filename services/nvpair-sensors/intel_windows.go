// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	_ "embed"
	"errors"
	"fmt"

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

// intelPackageSensor reads the package temperature: TjMax once at open, then
// IA32_PACKAGE_THERM_STATUS per sample. Package registers read the same from
// every core, so no thread affinity is needed.
type intelPackageSensor struct {
	dev    *pawnIO
	target uint32 // TjMax
}

// openIntelPackageSensor opens PawnIO, loads the Intel module and resolves
// TjMax. Every failure names the reason so the helper can publish it.
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
	return &intelPackageSensor{dev: dev, target: tjMax}, nil
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
