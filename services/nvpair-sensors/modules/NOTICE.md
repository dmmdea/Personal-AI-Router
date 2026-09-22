<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Embedded PawnIO modules

`IntelMSR.bin` is an unmodified, signed build of `IntelMSR.p` from [PawnIO.Modules](https://github.com/namazso/PawnIO.Modules),
release `0.2.11` (2026-08-30), asset `release_0_2_11.zip`.

| | |
| --- | --- |
| Copyright | (C) 2025 namazso <admin@namazso.eu> |
| License | GNU Lesser General Public License v2.1 or later (`COPYING` beside this file) |
| `IntelMSR.bin` SHA-256 | `d6ed85d65ab17a22f813ef98207d6d537155ee2ded5976a21cb48413c9b92e5f` |

A module is data handed to the PawnIO driver over device I/O control; it is not
linked into `nvpair-sensors`. The driver accepts only modules carrying the
PawnIO maintainer's signature, so a blob must be replaced by another signed
release, never rebuilt locally.

## `IntelMSR.bin` — the CPU package temperature

Exposes `ioctl_read_msr` / `ioctl_write_msr` over an allow list of thermal,
power and frequency registers. `nvpair-sensors` reads four of them and writes
none: `IA32_TEMPERATURE_TARGET` (0x1A2) and `IA32_PACKAGE_THERM_STATUS` (0x1B1)
for the package temperature, and `MSR_RAPL_POWER_UNIT` (0x606) and
`MSR_PKG_ENERGY_STATUS` (0x611) for the package power draw. All four are on the
module's `is_allowed_msr_read` list; the write list is much narrower and
contains none of them. `IA32_THERM_STATUS` (0x19C) is the per-core register and
is declared but not read — the package one is what this helper reports.

## Updating

Download the new release zip, verify the maintainer's release signature, copy
the `.bin` file here, and refresh the version and checksums above.
