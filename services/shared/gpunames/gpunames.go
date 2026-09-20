// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package gpunames turns an Intel or AMD PCI device id into the marketing
// name this project publishes for a GPU row.
//
// It exists because the two platforms reach the same silicon through
// completely different doors and used to disagree about what to call it. On
// Linux the DRM class hands us the raw PCI ids and nothing else, so a node
// list read "AMD Radeon Graphics" or nothing at all until the sysfs
// inventories grew these tables. On Windows DXGI hands us the driver's
// Description string, which is whatever the vendor's INF happened to say:
// "Intel(R) UHD Graphics" on a Tiger Lake laptop and "Intel(R) UHD Graphics
// 630" on a Coffee Lake desktop - two different generations, one of them not
// even identified by model. Neither string names the graphics architecture,
// which is the part an operator actually reasons about when deciding what a
// node can run.
//
// So the tables live here, keyed by PCI device id, and both platforms call
// the same lookup. A Windows row and a Linux row for the same part now read
// identically, and a new part only has to be added once.
//
// Every name renders as
//
//	<marketing name> (<codename>, <graphics architecture>)
//
// and every id was checked against a primary source rather than recalled:
//
//   - the kernel's own id header (include/drm/intel/pciids.h, which is what
//     i915 and xe bind on; historically drivers/gpu/drm/i915/i915_pciids.h
//     and drivers/gpu/drm/xe/xe_pciids.h, since merged into that one file)
//     for Intel, and the amdgpu PCI table for AMD;
//   - the PCI ID Repository's pci.ids for the marketing name;
//   - libdrm's data/amdgpu.ids for the AMD marketing names.
//
// Where the two disagree the rule is fixed, so the table stays reviewable:
// the marketing name comes from pci.ids when it has an entry, and the
// codename comes from the kernel macro the id is listed under, because that
// macro is what the driver actually binds on. An id the kernel lists but
// pci.ids does not gets the marketing name shared by the rest of its kernel
// macro group, and no model number at all when that group disagrees about
// one - a missing number is a smaller lie than a wrong one.
//
// Nothing here touches the wire contract: these strings only ever land in
// GPUInfo.Name, which has always been free-form.
package gpunames

import "strconv"

const (
	// IntelFallback is the vendor-level name an unlisted Intel adapter falls
	// back to, and the whole name for a card whose device id could not be read.
	IntelFallback = "Intel Graphics"

	// AMDFallback is the same for AMD. Both are deliberately the bare vendor
	// name: a row that cannot be identified must not pretend to be one that can.
	AMDFallback = "AMD Radeon Graphics"

	// PCIVendorIntel and PCIVendorAMD are the PCI vendor ids the callers gate
	// on. AMD's is shared with ATI, which is why an AMD GPU still reports
	// 0x1002.
	PCIVendorIntel = 0x8086
	PCIVendorAMD   = 0x1002
)

// ParseHexID parses a PCI device id written as bare hexadecimal without the
// "0x" prefix - the form Linux sysfs yields after the prefix is cut, e.g.
// "3e98". ok is false for an empty or malformed value, which callers turn
// into the bare vendor fallback rather than a nonsense lookup.
func ParseHexID(s string) (uint32, bool) {
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil || s == "" {
		return 0, false
	}
	return uint32(v), true
}

// deviceSuffix renders the "(device 0x3e98)" tail an unlisted part keeps, so
// a card released after these tables were written is still identifiable by
// anyone who can read an lspci line. The four-digit zero padding matches what
// sysfs prints, so a Linux row's name is byte-identical to what the string
// tables produced before the ids became numbers.
func deviceSuffix(deviceID uint32) string {
	return "(device 0x" + hex4(deviceID) + ")"
}

// hex4 formats a device id as at least four lowercase hex digits.
func hex4(deviceID uint32) string {
	s := strconv.FormatUint(uint64(deviceID), 16)
	for len(s) < 4 {
		s = "0" + s
	}
	return s
}
