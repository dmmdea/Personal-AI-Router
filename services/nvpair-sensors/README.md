<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# nvpair-sensors

The Windows host-sensor helper: a small LocalSystem service that reads the CPU
package temperature and the motherboard's own sensors, and serves both to
`nvpair-node-info` over a local named pipe, so the panel shows a CPU
temperature on Windows nodes as it does on Linux, and a row for the board.

## Why a service

Windows has no unprivileged CPU temperature API. The sensor is a
model-specific register (`IA32_PACKAGE_THERM_STATUS`, relative to
`IA32_TEMPERATURE_TARGET`) that only ring 0 can read; ACPI thermal zones,
where a board exposes any, are not the CPU. Every Windows monitor —
LibreHardwareMonitor, HWiNFO, vendor dashboards — reads it through a signed
kernel driver, and the current one, [PawnIO](https://pawnio.eu), admits
administrators and SYSTEM only. `nvpair-node-info` runs unprivileged under the
desktop app, so the read lives here.

The helper links nothing from the driver: it calls the documented
`PawnIOLib.dll` entry points, loads the signed `IntelMSR` module
(`modules/NOTICE.md`) and executes its `ioctl_read_msr`. PawnIO is a
prerequisite it does not install; without it the helper runs, reports why it
has no reading, and retries every 30 s.

## Wire contract

`nvpair-shared/hostsensors` defines it. Every connection to
`\\.\pipe\nvpair-sensors` receives one JSON document and is closed:

```json
{"schema":1,"helper_version":"0.1.0","cpu":{"package_celsius":58,"tjmax_celsius":110,"source":"intel-msr","sampled_at":"2026-09-19T21:40:12Z"},"board":{"chip":"Nuvoton NCT6798D","chip_id":"0xD4/0x2B","vendor":"ASUSTeK COMPUTER INC.","product":"ROG STRIX X299-E GAMING II","temperatures":[{"label":"CPU socket","input":"CPUTIN","role":"cpu-socket","celsius":46.5},{"label":"Motherboard","input":"SYSTIN","role":"motherboard","celsius":30}],"fans":[{"index":0,"label":"Fan 1","rpm":692}],"vcore_volts":0.912,"source":"superio-lpc","sampled_at":"2026-09-19T21:40:12Z"}}
```

or, when the helper has no sensor to read:

```json
{"schema":1,"helper_version":"0.1.0","error":"PawnIO is not installed (https://pawnio.eu), so there is no CPU package sensor to read"}
```

The pipe rejects remote clients and admits any authenticated local user. A
reader drops a report older than its freshness window rather than repeating a
frozen number, and treats a missing `cpu` exactly like an unreachable helper:
the temperature is omitted, never reported as zero.

`board` is a separate, optional section with the same rules, and `error`
describes the CPU sensor only — a host whose Super I/O chip this build does not
read still publishes its CPU temperature, and a host whose PawnIO install is
broken publishes neither. The two sensors are opened, retried and reopened
independently for exactly that reason.

`schema` counts breaking changes only. Adding `board` did not bump it on
purpose: the helper and its readers deploy separately, so a bump for an
additive section would make every already-installed reader refuse the whole
report and lose the CPU temperature it could still decode. New sections are
optional objects instead.

## The board sampler

A PC motherboard's own sensors live in a Super I/O chip on the LPC bus, behind
two x86 I/O ports that no user-mode process may touch. The helper reaches them
through a second signed PawnIO module, `LpcIO` (`modules/NOTICE.md`), which is
scoped to that chip: it discovers the port ranges the chip's logical devices
claim and refuses every port outside them.

What it does, once at open: enter the configuration window at 0x2E (then 0x4E),
read the chip id and revision, resolve the hardware-monitor window's base
address from logical device 0x0B, clear the Nuvoton I/O-space lock bit — the
single write this service makes to the chip — and confirm the window answers
with Nuvoton's vendor id. Then, every sample: read the temperature, tachometer
and voltage registers through the window's bank/index/data pair.

Supported chips are the Nuvoton NCT67xxD family — NCT6791D, NCT6792D(-A),
NCT6793D, NCT6795D, NCT6796D(-R), NCT6797D, NCT6798D, NCT6799D. A board with
anything else (an NCT668x embedded controller, an NCT610x, an ITE or Fintek
part) gets no `board` section and one Info log line naming what answered. The
decoding follows LibreHardwareMonitor's `Nct677X`/`LpcIO`, which is the
reference for this family.

### Arbitration, and a trap worth knowing about

Every tool that reads this chip takes the system-wide
`Global\Access_ISABUS.HTP.Method` mutant first, because a bank select and a
register read are not atomic. The helper takes it around each sample and
releases it between samples, so the vendor's fan-control service keeps working.

A Win32 mutant is owned by the **thread** that waited on it, and only that
thread may release it — while a goroutine is free to move between OS threads
whenever it blocks. Without `runtime.LockOSThread` around the hold, the release
lands on the wrong thread, fails with `ERROR_NOT_OWNER`, and the gate stays
held for the life of the process, locking out every other tool on the machine.
Measured on the target board: four seconds after the service started.

### What is reported, and what is not

Each temperature carries the chip's own input name (`CPUTIN`, `SYSTIN`, the
`AUXTIN`s, the PECI and PCH readouts) plus a `role` classifying what it
measures, so a reader can pick one without knowing the register map. Inputs
whose meaning is board-specific are `aux` and stay that way.

No input claims the `vrm` role from the chip alone. A Super I/O chip cannot
tell you whether a probe header actually has a probe in it: on the ASUS ROG
STRIX X299-E GAMING II (NCT6798D, BIOS 2103) the chip's own source register
declares the ASUS `T_Sensor` input connected, while that input reports a flat
88 °C that does not move between idle and eight threads of load, and reads
byte-for-byte identical to `AUXTIN4` on every sample. Publishing it as the
board's VRM temperature would have put a fabricated 88 °C on the node card.
The role stays in the contract for a producer that can establish it from
outside the chip.

An input that is not wired up floats and decodes to noise, so a reading at or
below zero, or above 125 °C, is dropped rather than published. A tachometer at
full scale is a real 0 RPM (an empty or stopped header); a count below what the
conversion can express is no reading and gets no row.

## Modes

| Invocation | Who | What |
| --- | --- | --- |
| `nvpair-sensors.exe --install` | administrator | Register this executable as the `nvpair-sensors` service (automatic start, LocalSystem, restart on failure) and start it |
| `nvpair-sensors.exe --uninstall` | administrator | Stop and remove the service |
| `nvpair-sensors.exe --probe` | any user | Read one report from the running service over the pipe and print it; exit 0 with a CPU reading, 2 without one, 1 when the helper is unreachable |
| `nvpair-sensors.exe --once` | administrator | Read the sensor directly and print one report (no service needed) |
| `nvpair-sensors.exe` | administrator | Run in the foreground until Ctrl-C (the same code path the service runs) |
| `--pipe`, `--interval` | | Pipe name (default above) and sampling interval (default 2 s) |
| `--version` | | Print version and exit |

In service mode the helper logs to the Windows event log under the source
`nvpair-sensors`: start, the sensor it opened (with TjMax), and one line per
change of reason when it cannot read.

## Supported sensors

- Intel x64: package temperature from the digital thermal sensor. TjMax is read
  once from `IA32_TEMPERATURE_TARGET`; each sample is `TjMax - readout`, taken
  only when the register's valid bit is set.
- AMD: not yet; the helper reports the vendor in its `error` and `node-info`
  omits the temperature, as before.
- Motherboard: temperatures, fan speeds and Vcore from a Nuvoton NCT67xxD
  Super I/O chip, on any board that has one — see "The board sampler" above.

## Other platforms

The binary exists only for Windows. Linux `nvpair-node-info` reads hwmon
directly; the non-Windows build answers `--version` and otherwise exits 2.

## Tests

`go test ./...` covers the register decoding against values captured live on an
i9-10980XE and an i7-11800H, and the contract package's encode/decode and
freshness rules. The Super I/O decoding runs against an in-memory chip that
speaks the real index/bank/data protocol, so a decoder that forgets the bank
switch fails in the test rather than on the bench. The PawnIO path itself is
exercised by running the helper: `--once` from an elevated prompt, then
`--probe` from a plain one.

A sensor that decodes in range is not necessarily a sensor. To tell them apart,
sample at idle, put the CPU under load for a minute, and sample again: the
inputs that measure something move, and the ones that are merely decoding sit
at exactly the same value. That is how the `T_Sensor` finding above was made.
