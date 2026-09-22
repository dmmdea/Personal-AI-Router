<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# nvpair-sensors

The Windows host-sensor helper: a small LocalSystem service that reads the CPU
package temperature and power draw, and serves them to `nvpair-node-info` over
a local named pipe, so the panel shows a CPU temperature on Windows nodes as it
does on Linux.

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
{"schema":1,"helper_version":"0.2.0","cpu":{"package_celsius":58,"package_watts":79,"tjmax_celsius":110,"source":"intel-msr","sampled_at":"2026-09-19T21:40:12Z"}}
```

or, when the helper has no sensor to read:

```json
{"schema":1,"helper_version":"0.1.0","error":"PawnIO is not installed (https://pawnio.eu), so there is no CPU package sensor to read"}
```

The pipe rejects remote clients and admits any authenticated local user. A
reader drops a report older than its freshness window rather than repeating a
frozen number, and treats a missing `cpu` exactly like an unreachable helper:
the temperature is omitted, never reported as zero.

`package_watts` is the package power draw in whole watts, and it is optional in
the same way: a part whose energy counter the helper cannot resolve publishes a
temperature and no wattage. The two readings come off the same open sensor on
the same tick and share one `sampled_at`, so a reader applying one freshness
window gets a consistent pair — never a wattage from one tick beside a
temperature from the next. A power read that fails is not reported as a sensor
fault, because that would trip the reopen path and cost the host the
temperature it came for.

`schema` counts breaking changes only. Adding `package_watts` did not bump it on
purpose: the helper and its readers
deploy separately, so a bump for an additive section — or an additive field
inside one — would make every already-installed reader refuse the whole report
and lose the CPU temperature it could still decode. New sections are optional
objects and new fields are `omitempty` instead.

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
- Intel x64: package **power** from RAPL. `MSR_RAPL_POWER_UNIT` (0x606) is read
  once for the energy scale (bits 12:8: energy is counted in 1/2^ESU joules) and
  `MSR_PKG_ENERGY_STATUS` (0x611) each sample. That register is a running
  32-bit total, so watts are the delta over the interval between two samples:
  the first sample after an open publishes nothing, and a delta that can only be
  a counter re-base is discarded and the baseline dropped. Both registers are on
  the signed `IntelMSR` module's read allow list beside the thermal ones (see
  `modules/NOTICE.md`), and both are package-scope, so — like the package
  thermal register — they read the same from every core and need no thread
  affinity, which the module exposes no way to request anyway. A part that
  refuses either register (a virtualized CPU that traps RAPL, a pre-Sandy-Bridge
  core) logs one line at open and publishes the temperature alone.
- AMD: not yet; the helper reports the vendor in its `error` and `node-info`
  omits the temperature, as before. PawnIO's `AMDFamily17` module does expose
  the Zen RAPL registers (`0xC0010299` / `0xC001029B`) on families 17h–1Ah, so
  the same derivation would work there once an AMD temperature path exists;
  this build embeds `IntelMSR` only.

## Other platforms

The binary exists only for Windows. Linux `nvpair-node-info` reads hwmon
directly; the non-Windows build answers `--version` and otherwise exits 2.

## Tests

`go test ./...` covers the register decoding — thermal and RAPL energy, wrap
handling included — against values captured live on an
i9-10980XE and an i7-11800H, and the contract package's encode/decode and
freshness rules. The PawnIO path itself is
exercised by running the helper: `--once` from an elevated prompt, then
`--probe` from a plain one.
