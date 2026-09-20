<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Embedded PawnIO module

`IntelMSR.bin` is an unmodified, signed build of `IntelMSR.p` from
[PawnIO.Modules](https://github.com/namazso/PawnIO.Modules), release
`0.2.11` (2026-08-30), asset `release_0_2_11.zip`.

| | |
| --- | --- |
| Copyright | (C) 2025 namazso <admin@namazso.eu> |
| License | GNU Lesser General Public License v2.1 or later (`COPYING` beside this file) |
| SHA-256 | `d6ed85d65ab17a22f813ef98207d6d537155ee2ded5976a21cb48413c9b92e5f` |

The module is data handed to the PawnIO driver over device I/O control; it is
not linked into `nvpair-sensors`. The driver accepts only modules carrying the
PawnIO maintainer's signature, so the blob must be replaced by another signed
release, never rebuilt locally. It exposes `ioctl_read_msr` / `ioctl_write_msr`
over an allow list of thermal, power and frequency registers; `nvpair-sensors`
reads three of them (`IA32_TEMPERATURE_TARGET`, `IA32_PACKAGE_THERM_STATUS`,
`IA32_THERM_STATUS`) and writes none.

To update: download the new release zip, verify the maintainer's release
signature, copy `IntelMSR.bin` here, refresh the version and checksum above.
