// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package gpunames

// amdModel is one known PCI device id: the name to publish, and whether the
// part is an APU (a VRAM carve-out plus a GTT aperture) rather than a discrete
// card. The apu flag is what the Linux inventory uses to decide how to add a
// row's capacity up, so it belongs to the table rather than to a separate guess.
type amdModel struct {
	name string
	apu  bool
}

// amdModels maps a PCI device id to the name this service publishes. Every
// entry renders as
//
//	<marketing name> (<codename>, <architecture>)
//
// The codename alone - which is all ghw and the PCI database report - tells a
// user nothing: "Barcelo" names neither the vendor nor the generation. The
// marketing name alone is barely better, because AMD has reused "AMD Radeon
// Graphics" across five architectures. The architecture is the part an operator
// actually reasons about when deciding what a node can run, so it is in the
// name rather than implied by a codename nobody memorizes.
//
// The shader-core (CU) count is deliberately absent. It is NOT derivable from
// the device id: 0x15e7 (Barcelo) alone ships as Vega 6, Vega 7 and Vega 8
// depending on the SKU's fused core count, and libdrm's amdgpu.ids needs the
// PCI *revision* id on top of the device id to tell those apart. An earlier
// table printed "Vega 7" for every 0x15e7, which was a guess.
//
// Every id below was checked against three sources rather than recalled: the
// kernel's amdgpu PCI table (drivers/gpu/drm/amd/amdgpu/amdgpu_drv.c), the PCI
// ID Repository's pci.ids, and libdrm's data/amdgpu.ids (which is where the
// marketing names come from). Three ids that are easy to transpose are called
// out because they were: 0x1900 is Hawk Point (Radeon 780M, RDNA 3) and NOT
// Strix Point, 0x150e is Strix Point (Radeon 890M) and NOT Strix Halo, and
// 0x1586 is Strix Halo (Radeon 8060S).
//
// The table is APU-only on purpose. A discrete Radeon is named by its driver
// on both platforms - DXGI reports "AMD Radeon RX 7900 XTX" on Windows and the
// Linux row carries the card's own VRAM figure - so there is nothing here for
// this table to improve, and an id list it cannot keep current would be worse
// than the vendor string it replaced.
//
// An unlisted id still produces a row - see AMDName - so a part released after
// this table was written is never dropped from an inventory.
var amdModels = map[uint32]amdModel{
	// GCN 5 (Vega, gfx900/902/909): the first Ryzen APUs.
	0x15dd: {"AMD Radeon Vega Graphics (Raven Ridge, GCN 5)", true},
	0x15d8: {"AMD Radeon Vega Graphics (Picasso, GCN 5)", true},

	// GCN 5.1 (gfx90c): Renoir and its three rebadges share one shader ISA.
	0x1636: {"AMD Radeon Vega Graphics (Renoir, GCN 5.1)", true},
	0x164c: {"AMD Radeon Vega Graphics (Lucienne, GCN 5.1)", true},
	0x1638: {"AMD Radeon Vega Graphics (Cezanne, GCN 5.1)", true},
	0x15e7: {"AMD Radeon Vega Graphics (Barcelo, GCN 5.1)", true},

	// RDNA 2 (gfx103x).
	0x164e: {"AMD Radeon Graphics (Raphael, RDNA 2)", true},
	0x1681: {"AMD Radeon 680M (Rembrandt, RDNA 2)", true},

	// RDNA 3 (gfx1103).
	0x15bf: {"AMD Radeon 780M (Phoenix, RDNA 3)", true},
	0x15c8: {"AMD Radeon 740M (Phoenix2, RDNA 3)", true},
	0x1900: {"AMD Radeon 780M (Hawk Point, RDNA 3)", true},

	// RDNA 3.5 (gfx115x).
	0x150e: {"AMD Radeon 890M (Strix Point, RDNA 3.5)", true},
	0x1586: {"AMD Radeon 8060S (Strix Halo, RDNA 3.5)", true},
	0x1114: {"AMD Radeon 860M (Krackan Point, RDNA 3.5)", true},
}

// amdGCArchitectures maps a Graphics Core IP {major, minor} to the
// architecture family AMD markets it as. It exists so a part released after
// amdModels was written still names its generation instead of showing a bare
// device id: amdgpu publishes the version the ASIC's own IP discovery table
// reported, so the generation stays knowable even when the name is not.
//
// The revision component is deliberately not part of the key: it separates
// steppings inside one family (9.3.0 and a later 9.3.x are both gfx90c), not
// families.
//
// GC 9.4.x (Arcturus / Aldebaran / MI300) and 9.5.x (MI350) are deliberately
// absent. They are the data-center CDNA line, not GCN 5.x, so the "9.x is
// GCN 5.x" shorthand would publish a wrong architecture on exactly the cards
// an operator would care most about. An unmapped version drops the
// architecture from the name rather than inventing one.
var amdGCArchitectures = map[[2]uint64]string{
	{9, 0}:  "GCN 5",
	{9, 1}:  "GCN 5",
	{9, 2}:  "GCN 5",
	{9, 3}:  "GCN 5.1",
	{10, 1}: "RDNA",
	{10, 3}: "RDNA 2",
	{11, 0}: "RDNA 3",
	{11, 5}: "RDNA 3.5",
	{12, 0}: "RDNA 4",
}

// AMD returns the published name for a listed AMD PCI device id. ok is false
// for an id the table does not carry, which is the caller's signal to keep
// whatever name its own platform already had (the DXGI description on Windows)
// or to fall back to AMDName (on Linux, where there is no other source).
func AMD(deviceID uint32) (string, bool) {
	m, ok := amdModels[deviceID]
	return m.name, ok
}

// AMDAPU reports whether a listed AMD part draws from a unified pool (a VRAM
// carve-out plus the GTT aperture) rather than dedicated memory. known is false
// for an unlisted id, which the caller then judges by the card's own numbers
// rather than by this table.
func AMDAPU(deviceID uint32) (apu, known bool) {
	m, ok := amdModels[deviceID]
	return m.apu, ok
}

// AMDArchitecture maps a Graphics Core IP {major, minor} pair - as amdgpu
// publishes it under a card's ip_discovery tree - to the architecture family
// AMD markets it as. ok is false for a version this table deliberately does not
// name, in which case the caller omits the architecture rather than guessing.
//
// Reading the version out of sysfs stays with the platform that has a sysfs;
// this package only maps the numbers, so it stays free of OS-specific code.
func AMDArchitecture(major, minor uint64) (string, bool) {
	arch, ok := amdGCArchitectures[[2]uint64{major, minor}]
	return arch, ok
}

// AMDName resolves an AMD device id to a display name, falling back rather
// than failing. haveID is false when the platform could not read a device id at
// all, which yields the bare vendor name.
//
// An unlisted id is never dropped and never published as a bare codename. It
// keeps its device id so the card is still identifiable, and it gains the
// architecture family whenever the caller could supply one from the driver's
// IP discovery table - which is the whole point of reading it: a node listing
// "AMD Radeon Graphics (device 0x1114, RDNA 3.5)" is useful on day one of a new
// part, where "Krackan" is not. An empty architecture leaves that clause out.
//
// The table is authoritative: a listed id keeps its verified name whatever the
// caller passes as architecture, so the published name never depends on which
// of two sources happened to be readable.
func AMDName(deviceID uint32, haveID bool, architecture string) string {
	if name, ok := AMD(deviceID); ok {
		return name
	}
	if !haveID {
		return AMDFallback
	}
	if architecture != "" {
		return AMDFallback + " (device 0x" + hex4(deviceID) + ", " + architecture + ")"
	}
	return AMDFallback + " " + deviceSuffix(deviceID)
}
