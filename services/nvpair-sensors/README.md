<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# nvpair-sensors

The Windows host-sensor helper: a small LocalSystem service that reads the CPU
package temperature and serves it to `nvpair-node-info` over a local named
pipe, so the panel shows a CPU temperature on Windows nodes as it does on
Linux.

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
{"schema":1,"helper_version":"0.1.0","cpu":{"package_celsius":58,"tjmax_celsius":110,"source":"intel-msr","sampled_at":"2026-09-19T21:40:12Z"}}
```

or, when the helper has no sensor to read:

```json
{"schema":1,"helper_version":"0.1.0","error":"PawnIO is not installed (https://pawnio.eu), so there is no CPU package sensor to read"}
```

The pipe rejects remote clients and admits any authenticated local user. A
reader drops a report older than its freshness window rather than repeating a
frozen number, and treats a missing `cpu` exactly like an unreachable helper:
the temperature is omitted, never reported as zero.

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

## Other platforms

The binary exists only for Windows. Linux `nvpair-node-info` reads hwmon
directly; the non-Windows build answers `--version` and otherwise exits 2.

## Tests

`go test ./...` covers the register decoding against values captured live on an
i9-10980XE and an i7-11800H, and the contract package's encode/decode and
freshness rules. The PawnIO path itself is exercised by running the helper:
`--once` from an elevated prompt, then `--probe` from a plain one.
