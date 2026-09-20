// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package gpunames

// intelModel is one known PCI device id: the name to publish, and whether the
// part is a discrete card (dedicated VRAM) rather than an integrated GPU
// sharing system DRAM. The discrete flag is what the Linux inventory uses to
// decide whether a row's capacity is the driver's VRAM figure or the host's
// system memory, so it belongs to the table rather than to a separate guess.
type intelModel struct {
	name     string
	discrete bool
}

// intelModels maps a PCI device id to the name this service publishes, in the
// same "<marketing name> (<codename>, <graphics architecture>)" shape the
// amdgpu table uses, so rows from the two vendors read alike in one node list.
//
// The codename is the family, not the kernel's package suffix: "Alder Lake",
// not "Alder Lake-S" / "-P" / "-N". The suffix describes which socket the same
// graphics IP was sold in, which tells an operator nothing about what the node
// can run, and pci.ids and the kernel macros do not always agree on it anyway
// (0xa7a1 is INTEL_RPLU_IDS to the kernel and "Raptor Lake-P" to pci.ids - the
// same part either way).
//
// Four results are worth knowing, because the obvious guess is wrong each time
// and each one was checked rather than recalled:
//
//   - 0x9a60/0x9a68/0x9a70 are INTEL_TGL_GT1_IDS, and pci.ids names them
//     "TigerLake-H GT1 [UHD Graphics]". They are not Iris Xe. Iris Xe on Tiger
//     Lake is the GT2 part (INTEL_TGL_GT2_IDS), listed separately below.
//   - 0x9a78 sits in that same GT2 group, but pci.ids names it specifically:
//     "Tiger Lake-LP GT2 [UHD Graphics G4]". A GT2 die with most of its EUs
//     fused off was sold as UHD Graphics, so this one id does NOT take the
//     "Iris Xe" string its group-mates take.
//   - 0x7d55 is "Meteor Lake-P [Intel Arc Graphics]" but 0x7dd5 is "Meteor
//     Lake-P [Intel Graphics]" - the same generation, sold under two names
//     depending on the Xe-core count, so they do not share a string.
//   - 0x9bf6 is INTEL_CML_GT2_IDS to the kernel and "Coffee Lake-S GT2" to
//     pci.ids. Both are Gen 9.5; the kernel's grouping decides the codename
//     here, because the kernel is what binds the driver.
//
// Arrow Lake is named Xe-LPG on the strength of the kernel's own wiring: the
// xe driver registers INTEL_ARL_IDS against mtl_desc, the Meteor Lake device
// descriptor. Same graphics IP family, not a guess.
//
// An unlisted id still produces a row - see IntelName - so a part released
// after this table was written is never dropped from an inventory.
var intelModels = map[uint32]intelModel{
	// Gen 9.5 (Coffee Lake). INTEL_CFL_*_IDS.
	0x3e90: {"Intel UHD Graphics 610 (Coffee Lake, Gen 9.5)", false},
	0x3e93: {"Intel UHD Graphics 610 (Coffee Lake, Gen 9.5)", false},
	0x3e99: {"Intel UHD Graphics 610 (Coffee Lake, Gen 9.5)", false},
	0x3e9c: {"Intel UHD Graphics 610 (Coffee Lake, Gen 9.5)", false},
	0x3ea9: {"Intel UHD Graphics 620 (Coffee Lake, Gen 9.5)", false},
	0x3e91: {"Intel UHD Graphics 630 (Coffee Lake, Gen 9.5)", false},
	0x3e92: {"Intel UHD Graphics 630 (Coffee Lake, Gen 9.5)", false},
	0x3e98: {"Intel UHD Graphics 630 (Coffee Lake, Gen 9.5)", false},
	0x3e9b: {"Intel UHD Graphics 630 (Coffee Lake, Gen 9.5)", false},
	0x3e94: {"Intel UHD Graphics P630 (Coffee Lake, Gen 9.5)", false},
	0x3e96: {"Intel UHD Graphics P630 (Coffee Lake, Gen 9.5)", false},
	0x3e9a: {"Intel UHD Graphics P630 (Coffee Lake, Gen 9.5)", false},
	0x3ea6: {"Intel Iris Plus Graphics 645 (Coffee Lake, Gen 9.5)", false},
	0x3ea5: {"Intel Iris Plus Graphics 655 (Coffee Lake, Gen 9.5)", false},
	0x3ea8: {"Intel Iris Plus Graphics 655 (Coffee Lake, Gen 9.5)", false},
	// 0x3ea7 is INTEL_CFL_U_GT3_IDS but absent from pci.ids, and its group
	// holds both 645 and 655 parts - so it carries no model number.
	0x3ea7: {"Intel Iris Plus Graphics (Coffee Lake, Gen 9.5)", false},

	// Gen 9.5 (Amber Lake). INTEL_AML_CFL_GT2_IDS.
	0x87ca: {"Intel UHD Graphics 617 (Amber Lake, Gen 9.5)", false},

	// Gen 9.5 (Whiskey Lake). INTEL_WHL_U_*_IDS; 0x3ea2/0x3ea3/0x3ea4 are
	// kernel-only, named from the rest of their GT group.
	0x3ea1: {"Intel UHD Graphics 610 (Whiskey Lake, Gen 9.5)", false},
	0x3ea4: {"Intel UHD Graphics 610 (Whiskey Lake, Gen 9.5)", false},
	0x3ea0: {"Intel UHD Graphics 620 (Whiskey Lake, Gen 9.5)", false},
	0x3ea3: {"Intel UHD Graphics 620 (Whiskey Lake, Gen 9.5)", false},
	0x3ea2: {"Intel Iris Plus Graphics (Whiskey Lake, Gen 9.5)", false},

	// Gen 9.5 (Comet Lake). INTEL_CML_*_IDS.
	0x9ba2: {"Intel UHD Graphics 610 (Comet Lake, Gen 9.5)", false},
	0x9ba4: {"Intel UHD Graphics 610 (Comet Lake, Gen 9.5)", false},
	0x9ba5: {"Intel UHD Graphics 610 (Comet Lake, Gen 9.5)", false},
	0x9ba8: {"Intel UHD Graphics 610 (Comet Lake, Gen 9.5)", false},
	0x9b21: {"Intel UHD Graphics 620 (Comet Lake, Gen 9.5)", false},
	0x9bc5: {"Intel UHD Graphics 630 (Comet Lake, Gen 9.5)", false},
	0x9bc8: {"Intel UHD Graphics 630 (Comet Lake, Gen 9.5)", false},
	0x9bc6: {"Intel UHD Graphics P630 (Comet Lake, Gen 9.5)", false},
	0x9be6: {"Intel UHD Graphics P630 (Comet Lake, Gen 9.5)", false},
	0x9bf6: {"Intel UHD Graphics P630 (Comet Lake, Gen 9.5)", false},
	0x9b41: {"Intel UHD Graphics (Comet Lake, Gen 9.5)", false},
	0x9baa: {"Intel UHD Graphics (Comet Lake, Gen 9.5)", false},
	0x9bac: {"Intel UHD Graphics (Comet Lake, Gen 9.5)", false},
	0x9bc2: {"Intel UHD Graphics (Comet Lake, Gen 9.5)", false},
	0x9bc4: {"Intel UHD Graphics (Comet Lake, Gen 9.5)", false},
	0x9bca: {"Intel UHD Graphics (Comet Lake, Gen 9.5)", false},
	0x9bcc: {"Intel UHD Graphics (Comet Lake, Gen 9.5)", false},

	// Xe-LP (Rocket Lake). INTEL_RKL_IDS; 0x4c80/0x4c8c are kernel-only.
	0x4c8a: {"Intel UHD Graphics 750 (Rocket Lake, Xe-LP)", false},
	0x4c8b: {"Intel UHD Graphics 730 (Rocket Lake, Xe-LP)", false},
	0x4c90: {"Intel UHD Graphics P750 (Rocket Lake, Xe-LP)", false},
	0x4c80: {"Intel UHD Graphics (Rocket Lake, Xe-LP)", false},
	0x4c8c: {"Intel UHD Graphics (Rocket Lake, Xe-LP)", false},
	0x4c9a: {"Intel UHD Graphics (Rocket Lake, Xe-LP)", false},

	// Xe-LP (Tiger Lake). GT1 is UHD Graphics, GT2 is Iris Xe - except 0x9a78,
	// which pci.ids names explicitly. 0x9a59/0x9ac0/0x9ac9/0x9ad9/0x9af8 are
	// kernel-only GT2 ids.
	0x9a60: {"Intel UHD Graphics (Tiger Lake, Xe-LP)", false},
	0x9a68: {"Intel UHD Graphics (Tiger Lake, Xe-LP)", false},
	0x9a70: {"Intel UHD Graphics (Tiger Lake, Xe-LP)", false},
	0x9a78: {"Intel UHD Graphics G4 (Tiger Lake, Xe-LP)", false},
	0x9a40: {"Intel Iris Xe Graphics (Tiger Lake, Xe-LP)", false},
	0x9a49: {"Intel Iris Xe Graphics (Tiger Lake, Xe-LP)", false},
	0x9a59: {"Intel Iris Xe Graphics (Tiger Lake, Xe-LP)", false},
	0x9ac0: {"Intel Iris Xe Graphics (Tiger Lake, Xe-LP)", false},
	0x9ac9: {"Intel Iris Xe Graphics (Tiger Lake, Xe-LP)", false},
	0x9ad9: {"Intel Iris Xe Graphics (Tiger Lake, Xe-LP)", false},
	0x9af8: {"Intel Iris Xe Graphics (Tiger Lake, Xe-LP)", false},

	// Xe-LP (DG1 / SG1): Intel's first modern discrete parts.
	0x4905: {"Intel Iris Xe MAX Graphics (DG1, Xe-LP)", true},
	0x4906: {"Intel Iris Xe Pod (DG1, Xe-LP)", true},
	0x4907: {"Intel Server GPU SG-18M (SG1, Xe-LP)", true},
	0x4908: {"Intel Iris Xe Graphics (DG1, Xe-LP)", true},
	0x4909: {"Intel Iris Xe MAX 100 (DG1, Xe-LP)", true},

	// Xe-LP (Alder Lake). INTEL_ADLS_IDS / INTEL_ADLP_IDS / INTEL_ADLN_IDS.
	// An id pci.ids does not name keeps the bare vendor string rather than
	// borrowing a model number off a group-mate.
	0x4680: {"Intel UHD Graphics 770 (Alder Lake, Xe-LP)", false},
	0x4688: {"Intel UHD Graphics 770 (Alder Lake, Xe-LP)", false},
	0x4690: {"Intel UHD Graphics 770 (Alder Lake, Xe-LP)", false},
	0x4682: {"Intel UHD Graphics 730 (Alder Lake, Xe-LP)", false},
	0x4692: {"Intel UHD Graphics 730 (Alder Lake, Xe-LP)", false},
	0x4693: {"Intel UHD Graphics 710 (Alder Lake, Xe-LP)", false},
	0x468a: {"Intel UHD Graphics (Alder Lake, Xe-LP)", false},
	0x468b: {"Intel UHD Graphics (Alder Lake, Xe-LP)", false},
	0x46a1: {"Intel UHD Graphics (Alder Lake, Xe-LP)", false},
	0x46a3: {"Intel UHD Graphics (Alder Lake, Xe-LP)", false},
	0x462a: {"Intel UHD Graphics (Alder Lake, Xe-LP)", false},
	0x4628: {"Intel UHD Graphics (Alder Lake, Xe-LP)", false},
	0x46b3: {"Intel UHD Graphics (Alder Lake, Xe-LP)", false},
	0x46c3: {"Intel UHD Graphics (Alder Lake, Xe-LP)", false},
	0x46d0: {"Intel UHD Graphics (Alder Lake, Xe-LP)", false},
	0x46d1: {"Intel UHD Graphics (Alder Lake, Xe-LP)", false},
	0x46d2: {"Intel UHD Graphics (Alder Lake, Xe-LP)", false},
	0x46a0: {"Intel Iris Xe Graphics (Alder Lake, Xe-LP)", false},
	0x46a6: {"Intel Iris Xe Graphics (Alder Lake, Xe-LP)", false},
	0x46a8: {"Intel Iris Xe Graphics (Alder Lake, Xe-LP)", false},
	0x46aa: {"Intel Iris Xe Graphics (Alder Lake, Xe-LP)", false},
	0x46b0: {"Intel Iris Xe Graphics (Alder Lake, Xe-LP)", false},
	0x46b1: {"Intel Iris Xe Graphics (Alder Lake, Xe-LP)", false},
	0x46c1: {"Intel Iris Xe Graphics (Alder Lake, Xe-LP)", false},
	0x4626: {"Intel Graphics (Alder Lake, Xe-LP)", false},
	0x46a2: {"Intel Graphics (Alder Lake, Xe-LP)", false},
	0x46b2: {"Intel Graphics (Alder Lake, Xe-LP)", false},
	0x46c0: {"Intel Graphics (Alder Lake, Xe-LP)", false},
	0x46c2: {"Intel Graphics (Alder Lake, Xe-LP)", false},
	0x46d3: {"Intel Graphics (Alder Lake, Xe-LP)", false},
	0x46d4: {"Intel Graphics (Alder Lake, Xe-LP)", false},

	// Xe-LP (Raptor Lake). INTEL_RPLS_IDS / INTEL_RPLP_IDS / INTEL_RPLU_IDS.
	0xa780: {"Intel UHD Graphics 770 (Raptor Lake, Xe-LP)", false},
	0xa781: {"Intel UHD Graphics (Raptor Lake, Xe-LP)", false},
	0xa782: {"Intel UHD Graphics (Raptor Lake, Xe-LP)", false},
	0xa783: {"Intel UHD Graphics (Raptor Lake, Xe-LP)", false},
	0xa788: {"Intel UHD Graphics (Raptor Lake, Xe-LP)", false},
	0xa789: {"Intel UHD Graphics (Raptor Lake, Xe-LP)", false},
	0xa78a: {"Intel UHD Graphics (Raptor Lake, Xe-LP)", false},
	0xa78b: {"Intel UHD Graphics (Raptor Lake, Xe-LP)", false},
	0xa720: {"Intel UHD Graphics (Raptor Lake, Xe-LP)", false},
	0xa721: {"Intel UHD Graphics (Raptor Lake, Xe-LP)", false},
	0xa7a8: {"Intel UHD Graphics (Raptor Lake, Xe-LP)", false},
	0xa7a9: {"Intel UHD Graphics (Raptor Lake, Xe-LP)", false},
	0xa7a0: {"Intel Iris Xe Graphics (Raptor Lake, Xe-LP)", false},
	0xa7a1: {"Intel Iris Xe Graphics (Raptor Lake, Xe-LP)", false},
	0xa7aa: {"Intel Graphics (Raptor Lake, Xe-LP)", false},
	0xa7ab: {"Intel Graphics (Raptor Lake, Xe-LP)", false},
	0xa7ac: {"Intel Graphics (Raptor Lake, Xe-LP)", false},
	0xa7ad: {"Intel Graphics (Raptor Lake, Xe-LP)", false},

	// Xe-HPG (Alchemist / DG2): the first discrete Arc cards, plus the
	// ATS-M data-center parts cut from the same silicon.
	0x56a0: {"Intel Arc A770 (Alchemist, Xe-HPG)", true},
	0x56a1: {"Intel Arc A750 (Alchemist, Xe-HPG)", true},
	0x56a2: {"Intel Arc A580 (Alchemist, Xe-HPG)", true},
	0x56a5: {"Intel Arc A380 (Alchemist, Xe-HPG)", true},
	0x56a6: {"Intel Arc A310 (Alchemist, Xe-HPG)", true},
	0x56a3: {"Intel Arc Xe Graphics (Alchemist, Xe-HPG)", true},
	0x56a4: {"Intel Arc Xe Graphics (Alchemist, Xe-HPG)", true},
	0x56be: {"Intel Arc A750E (Alchemist, Xe-HPG)", true},
	0x56bf: {"Intel Arc A580E (Alchemist, Xe-HPG)", true},
	0x56ba: {"Intel Arc A380E (Alchemist, Xe-HPG)", true},
	0x56bb: {"Intel Arc A310E (Alchemist, Xe-HPG)", true},
	0x56bc: {"Intel Arc A370E (Alchemist, Xe-HPG)", true},
	0x56bd: {"Intel Arc A350E (Alchemist, Xe-HPG)", true},
	0x5690: {"Intel Arc A770M (Alchemist, Xe-HPG)", true},
	0x5691: {"Intel Arc A730M (Alchemist, Xe-HPG)", true},
	0x5692: {"Intel Arc A550M (Alchemist, Xe-HPG)", true},
	0x5693: {"Intel Arc A370M (Alchemist, Xe-HPG)", true},
	0x5694: {"Intel Arc A350M (Alchemist, Xe-HPG)", true},
	0x5695: {"Intel Iris Xe MAX A200M (Alchemist, Xe-HPG)", true},
	0x5696: {"Intel Arc A570M (Alchemist, Xe-HPG)", true},
	0x5697: {"Intel Arc A530M (Alchemist, Xe-HPG)", true},
	0x56b0: {"Intel Arc Pro A30M (Alchemist, Xe-HPG)", true},
	0x56b1: {"Intel Arc Pro A40/A50 (Alchemist, Xe-HPG)", true},
	0x56b2: {"Intel Arc Pro A60M (Alchemist, Xe-HPG)", true},
	0x56b3: {"Intel Arc Pro A60 (Alchemist, Xe-HPG)", true},
	0x56c0: {"Intel Data Center GPU Flex 170 (ATS-M, Xe-HPG)", true},
	0x56c1: {"Intel Data Center GPU Flex 140 (ATS-M, Xe-HPG)", true},
	0x56c2: {"Intel Data Center GPU Flex 170V (ATS-M, Xe-HPG)", true},

	// Xe-LPG (Meteor Lake).
	0x7d55: {"Intel Arc Graphics (Meteor Lake, Xe-LPG)", false},
	0x7d40: {"Intel Graphics (Meteor Lake, Xe-LPG)", false},
	0x7d45: {"Intel Graphics (Meteor Lake, Xe-LPG)", false},
	0x7d60: {"Intel Graphics (Meteor Lake, Xe-LPG)", false},
	0x7dd5: {"Intel Graphics (Meteor Lake, Xe-LPG)", false},

	// Xe-LPG (Arrow Lake): INTEL_ARL_IDS, registered against the Meteor Lake
	// device descriptor by the xe driver.
	0x7d51: {"Intel Arc Pro 130T/140T (Arrow Lake, Xe-LPG)", false},
	0x7d41: {"Intel Graphics (Arrow Lake, Xe-LPG)", false},
	0x7d67: {"Intel Graphics (Arrow Lake, Xe-LPG)", false},
	0x7dd1: {"Intel Graphics (Arrow Lake, Xe-LPG)", false},
	0xb640: {"Intel Graphics (Arrow Lake, Xe-LPG)", false},

	// Xe2 (Lunar Lake integrated).
	0x64a0: {"Intel Arc Graphics 130V/140V (Lunar Lake, Xe2)", false},
	0x6420: {"Intel Graphics (Lunar Lake, Xe2)", false},
	0x64b0: {"Intel Graphics (Lunar Lake, Xe2)", false},

	// Xe2-HPG (Battlemage discrete). 0xe209 is kernel-only.
	0xe20b: {"Intel Arc B580 (Battlemage, Xe2-HPG)", true},
	0xe20c: {"Intel Arc B570 (Battlemage, Xe2-HPG)", true},
	0xe211: {"Intel Arc Pro B60 (Battlemage, Xe2-HPG)", true},
	0xe212: {"Intel Arc Pro B50 (Battlemage, Xe2-HPG)", true},
	0xe222: {"Intel Arc Pro B65 (Battlemage, Xe2-HPG)", true},
	0xe223: {"Intel Arc Pro B70 (Battlemage, Xe2-HPG)", true},
	0xe202: {"Intel Graphics (Battlemage, Xe2-HPG)", true},
	0xe209: {"Intel Graphics (Battlemage, Xe2-HPG)", true},
	0xe20d: {"Intel Graphics (Battlemage, Xe2-HPG)", true},
	0xe210: {"Intel Graphics (Battlemage, Xe2-HPG)", true},
	0xe216: {"Intel Graphics (Battlemage, Xe2-HPG)", true},
	0xe220: {"Intel Graphics (Battlemage, Xe2-HPG)", true},
	0xe221: {"Intel Graphics (Battlemage, Xe2-HPG)", true},
}

// Intel returns the published name for a listed Intel PCI device id. ok is
// false for an id the table does not carry, which is the caller's signal to
// keep whatever name its own platform already had (the DXGI description on
// Windows) or to fall back to IntelName (on Linux, where there is no other
// source).
func Intel(deviceID uint32) (string, bool) {
	m, ok := intelModels[deviceID]
	return m.name, ok
}

// IntelDiscrete reports whether a listed Intel part has dedicated VRAM. known
// is false for an unlisted id, which the caller then judges by the card's own
// numbers rather than by this table.
func IntelDiscrete(deviceID uint32) (discrete, known bool) {
	m, ok := intelModels[deviceID]
	return m.discrete, ok
}

// IntelName resolves an Intel device id to a display name, falling back rather
// than failing. haveID is false when the platform could not read a device id at
// all, which yields the bare vendor name; an unlisted id keeps its number in
// the name, so a part released after intelModels was written still appears in
// an inventory and is still identifiable by anyone who can read an lspci line.
func IntelName(deviceID uint32, haveID bool) string {
	if name, ok := Intel(deviceID); ok {
		return name
	}
	if !haveID {
		return IntelFallback
	}
	return IntelFallback + " " + deviceSuffix(deviceID)
}
