<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Embedded PawnIO modules

`IntelMSR.bin` and `LpcIO.bin` are unmodified, signed builds of `IntelMSR.p`
and `LpcIO.p` from [PawnIO.Modules](https://github.com/namazso/PawnIO.Modules),
release `0.2.11` (2026-08-30), asset `release_0_2_11.zip`.

| | |
| --- | --- |
| Copyright | (C) 2025 namazso <admin@namazso.eu> |
| License | GNU Lesser General Public License v2.1 or later (`COPYING` beside this file) |
| `IntelMSR.bin` SHA-256 | `d6ed85d65ab17a22f813ef98207d6d537155ee2ded5976a21cb48413c9b92e5f` |
| `LpcIO.bin` SHA-256 | `b3896a1cab0d808fca31fe2ebcae045d59dac690da87b17c858bb8da357eb45e` |

A module is data handed to the PawnIO driver over device I/O control; neither is
linked into `nvpair-sensors`. The driver accepts only modules carrying the
PawnIO maintainer's signature, so a blob must be replaced by another signed
release, never rebuilt locally.

## `IntelMSR.bin` — the CPU package temperature

Exposes `ioctl_read_msr` / `ioctl_write_msr` over an allow list of thermal,
power and frequency registers. `nvpair-sensors` reads three of them
(`IA32_TEMPERATURE_TARGET`, `IA32_PACKAGE_THERM_STATUS`, `IA32_THERM_STATUS`)
and writes none.

## `LpcIO.bin` — the motherboard sensors

Exposes the Super I/O chip on the LPC bus: `ioctl_select_slot` picks the
configuration window (0x2E or 0x4E), `ioctl_superio_inb` / `_inw` / `_outb`
address it, `ioctl_find_bars` walks the chip's logical devices to learn which
port ranges it claims, and `ioctl_pio_inb` / `ioctl_pio_outb` then read and
write within those ranges and nowhere else — the module refuses any other
port, including the PCI configuration ports.

`nvpair-sensors` uses it to identify the chip, resolve its hardware-monitor
window and read that window's temperature, tachometer and voltage registers
(see `../superio.go`). It makes exactly one write to the chip: clearing the
Nuvoton I/O-space lock bit in configuration register 0x28, without which every
monitor register reads back as `0xFF`. It writes no fan, no voltage and no
clock register.

The module's documentation notes that a caller should hold the system-wide
`\BaseNamedObjects\Access_ISABUS.HTP.Method` mutant around these calls;
`../board_windows.go` does, and releases it between samples so the vendor's own
tools keep working.

## Updating

Download the new release zip, verify the maintainer's release signature, copy
the `.bin` files here, and refresh the version and checksums above.
