<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# nvpair-node-info

A Go service that exposes this machine's hardware inventory (GPUs, CPU, physical memory) over a small HTTP API at `/v1/node-info`. It advertises nothing over mDNS itself — its parent (the broker) registers its `ni` port with the `nvpair-node-scanner` discovery daemon, which folds it into this node's single `_nvpair-node` record and fetches `/v1/node-info` to enrich the node for peers.

## Communication

Two surfaces:

- **HTTP(S)** — serves the node inventory at `/v1/node-info`. Plaintext HTTP on `:14318` by default; optional HTTPS (with optional mTLS) on `:14319` when a cert/key pair is supplied.
- **stdio JSON-RPC 2.0** — newline-delimited, used for lifecycle/control (`log/set-level`, `nodeinfo:set-cluster-identity`) and shutdown via stdin EOF. The service is normally launched as a subprocess by the broker.

It does **not** advertise itself over mDNS. Discovery is centralized in the `nvpair-node-scanner` daemon: the broker registers this service's `ni` port with the daemon, which carries it on the node's one `_nvpair-node` record and fetches `/v1/node-info` over plain HTTP to enrich each node.

## CLI Flags

| Flag | Default | Description |
|---|---|---|
| `--port <n>` | `14318` | HTTP port to listen on |
| `--tls-port <n>` | `14319` | HTTPS port (only used when `--cert` and `--key` are set) |
| `--cert <path>` | _(none)_ | TLS server certificate (PEM); enables HTTPS when set together with `--key` |
| `--key <path>` | _(none)_ | TLS server private key (PEM); enables HTTPS when set together with `--cert` |
| `--client-ca <path>` | _(none)_ | PEM bundle of CAs trusted to sign client certificates; enables mTLS when set |
| `--accept-http` | `false` | Keep the plaintext HTTP listener alive even when TLS is enabled (default: HTTPS-only once `--cert` is set) |
| `--cluster-dir <path>` | _(none)_ | Cluster config dir (`node.crt`/`node.key` + `trusted/`); when set, the primary port carries one cluster-gated listener. While this node is a cluster member, `/v1/node-info` is served **only** over cluster-scoped pin-based mTLS, to pinned cluster peers — or this node itself via self-trust — and a plaintext caller is refused with `403`; while it is not a member the plaintext inventory is served as usual. Membership is re-read per request. Takes precedence over the `--cert` path |
| `--version` | | Print version and exit |

`--log-level` is also accepted (registered by `nvpair-shared/applog`).

### TLS / mTLS configuration

The cert flags follow a bring-your-own contract, validated at startup:

- `--cert` and `--key` must be supplied together; either alone is rejected.
- `--client-ca` requires `--cert` and `--key`. When set, the server requires and verifies a client certificate (`RequireAndVerifyClientCert`); connections without a trusted client cert are dropped at handshake time.
- TLS minimum version is pinned to TLS 1.2.

When TLS is enabled, the HTTP listener is dropped unless `--accept-http` is passed. Both listeners share the same handler, but bind separate ports so clients can migrate independently.

`--cluster-dir` replaces both with a single cluster-gated listener on the primary port (see the flag table): one bound port, both personalities, and the choice between them follows live cluster membership rather than being fixed when the listener was bound.

## HTTP API

### `GET /v1/node-info`

Returns the merged static identity (collected once at startup) and the latest dynamic stats snapshot.

```json
{
  "GPUs": [
    {
      "name": "NVIDIA GeForce RTX 3080",
      "vram_bytes": 10737418240,
      "vram_used_bytes": 2147483648,
      "utilization_percent": 42,
      "temperature_celsius": 61,
      "power_watts": 210
    },
    {
      "name": "Google Coral Edge TPU",
      "utilization_percent": 30,
      "kind": "npu",
      "temperature_celsius": 52,
      "inference_ready": false
    },
    {
      "name": "Intel UHD Graphics 630 (Coffee Lake, Gen 9.5)",
      "vram_bytes": 67282386944,
      "memory_pool": "unified",
      "utilization_unavailable": true,
      "inference_ready": false
    }
  ],
  "telemetryValid": true,
  "msSince": 137,
  "cpu": {
    "name": "AMD Ryzen 9 5900X 12-Core Processor",
    "cores": 12,
    "utilization_percent": 7,
    "temperature_celsius": 58,
    "power_watts": 79
  },
  "memory": {
    "total_bytes": 34359738368,
    "used_bytes": 12884901888
  },
  "hostUuid": "8661676a-0d1c-4bd3-ac5e-4d370e6f1a9c",
  "clusterUuid": ""
}
```

Field notes:

- Dynamic fields (`vram_used_bytes`, `utilization_percent`, `cpu.utilization_percent`, `memory.used_bytes`) are populated by the Windows, Linux, and macOS per-tick stats collectors.
- `telemetryValid` and `msSince` describe the node-wide GPU utilization snapshot. `telemetryValid` is `true` after the collector has produced a usable GPU sample; `msSince` is that sample's age in milliseconds at response time. A failed collection retains the last usable sample and lets its age increase. Before the first usable sample, and on platforms without dynamic GPU telemetry, the response reports `telemetryValid:false` and `msSince:0`; consumers must ignore the age while validity is false.
- `clusterUuid` is the cluster principal this node currently holds. It has three distinct states on the wire: **absent** means unknown, **present and empty** means this node belongs to no cluster, and a value is that principal. A consumer must not read absent as unclustered — that is how a node too old to report the field answers, and also how this node answers before its parent has told it anything, so acting on it would clear a correct annotation elsewhere in the fleet.
- Under the broker, `clusterUuid` is pushed in over stdin (`nodeinfo:set-cluster-identity`) because node-info is spawned with no cluster dir and so cannot read membership itself; the field stays absent until the first push arrives. Standalone with `--cluster-dir`, it reads membership from the trust store per request instead and is therefore always known. The two sources are mutually exclusive by deployment, not a fallback chain.
- `clusterUuid` exists so a peer can learn this node's membership without its mDNS record. Membership otherwise travels only as the `cluster-uuid=` TXT key, which a consumer reads once per record *change*; a consumer that misses that change keeps the previous value indefinitely, and one still holding a departed node's principal will suppress the invite that would bring it back.
- `kind` is absent for a GPU, `"npu"` for a dedicated inference accelerator (an Edge TPU / NPU). Both are listed in the same `GPUs` inventory so every client shows them, and none of them can run the engines PAIR schedules — so consumers derive a node's GPU pressure with `noderec.MaxGPUUtilization`, which counts **only** rows with an empty `kind`, and no such row contributes to `telemetryValid` / `msSince`. The rule is "GPUs only" rather than a list of the kinds that existed when it was written, so a kind added later is skipped without anyone having to remember.
- `temperature_celsius` is a device's thermal readout in whole degrees: on a GPU row from `nvidia-smi` (`temperature.gpu`, Linux and Windows; joined to the adapter by PCI address on Windows), on an accelerator row from its driver, and on `cpu` the package temperature from Linux hwmon (`coretemp` "Package id 0" / `k10temp` Tctl, else the `x86_pkg_temp` thermal zone). On Windows the package sensor is a ring-0 register, so the reading comes from the elevated `nvpair-sensors` service over `\\.\pipe\nvpair-sensors` (see `../nvpair-sensors/README.md`); a host without that service, without PawnIO, or with a stale report omits it. Every temperature field is omitted wherever it cannot be read.
- `power_watts` is what a device is drawing in whole watts, published only where the hardware meters itself: an NVIDIA GPU (`nvidia-smi`'s `power.draw`, Linux and Windows), an AMD discrete GPU (the amdgpu hwmon's `PPT` input), and on `cpu` the package power — derived from the processor's energy counter, or on a Linux AMD APU host from that APU's `PPT` input, which meters the whole socket. Every other row has no meter and carries no such field — see **Power draw** below for the full table and for why a Linux host normally reports no `cpu.power_watts`.
- All dynamic fields and the `cpu` / `memory` objects use `omitempty`: a value the service couldn't read is dropped from the JSON entirely rather than reported as a misleading literal zero. A genuinely idle CPU renders the same as "unknown" — that ambiguity is intentional and benign.
- `utilization_unavailable` is `true` on a device row that has **no busy counter** this service can read, and absent everywhere else. It exists because `utilization_percent` is `omitempty`: a measured idle `0` is absent on the wire too, so the absence alone cannot tell "idle" from "cannot tell", and a client that renders both as "0 %" claims an idle device nobody measured. Set on a Hailo module whose inference process is not publishing a usable, current activity file (HailoRT has no busy counter on Windows; see [Utilization from the activity file](#utilization-from-the-activity-file)), on every Linux Intel GPU (i915/xe keep theirs in a root-gated PMU), and on a Mali GPU or RKNPU row whose sampler has not read its counter — before the first read, or after three consecutive failed reads, rather than freezing the last value. A client must show such a row's usage as unknown; a row without the flag keeps its historical meaning, so an idle GPU on a node that predates the field still reads 0 %.
- `inference_ready` is `false` on a device no engine PAIR runs can use — an Arm Mali GPU, an RKNPU, a Google Coral Edge TPU, a Hailo module, a Linux Intel integrated GPU — and absent on every other row, which makes no claim either way. It travels per row rather than as a node-level id list because clients re-sort the rows and build their own ids. The desktop derives the node's inference-ready list from it, so a board whose only devices are a Mali GPU and an NPU shows its CPU and RAM instead of two idle GPU rings.
- `memory.total_bytes` and `memory.used_bytes` are on **one base**: the memory the operating system manages, read from the same source the used figure is — `/proc/meminfo` `MemTotal` on Linux (used = `MemTotal - MemAvailable`), `GlobalMemoryStatusEx` `TotalPhys` on Windows (used = `TotalPhys - AvailPhys`), gopsutil's Mach readers on macOS. The Linux Intel, Mali, RKNPU and NVIDIA UMA rows use this same total as their `vram_bytes` ceiling; a Windows integrated GPU does not (see `memory_pool` below), because this base excludes the firmware-reserved memory an APU's graphics carve-out lives in. Installed DIMM capacity is not used: on Windows it counts hardware-reserved memory that can never appear as used, and on Linux it is not readable unprivileged. The Linux total used to be the count of online memory blocks times the block size, which counts the blocks around the PCI hole and at the top of RAM in full: a 64 GiB desktop with 2 GiB blocks published 66 GiB against a `MemTotal` of 62.7 GiB, and RAM % read two points low.
- `vram_bytes` is reported through DXGI on Windows, `nvidia-smi` on Linux, and IORegistry on macOS. On a unified-memory NVIDIA GPU such as DGX Spark, Linux uses total physical system memory for `vram_bytes` and the independently sampled system-memory usage for `vram_used_bytes`. On Apple Silicon, `vram_bytes` is total physical unified memory and `vram_used_bytes` is the GPU driver's mapped allocation (`Alloc system memory`), not whole-system RAM usage or the momentarily active subset.
- `memory_pool` is absent on a device with memory of its own and `"unified"` on one whose `vram_bytes` is a pool it shares with the host. A client must not label a unified capacity "VRAM", and must not assume such a row reports usage — see **Unified memory rows** below. On Windows an Intel or AMD integrated adapter is classified by the same PCI id table the Linux inventory uses (`nvpair-shared/gpunames`), and an id the table does not list by DXCore's `IsIntegrated` adapter property; such a row's `vram_bytes` is DXGI's `DedicatedVideoMemory + SharedSystemMemory` and its `vram_used_bytes` is PDH's `Dedicated Usage + Shared Usage` — the Windows counterpart of amdgpu's VRAM + GTT on Linux. The dedicated half alone is only the firmware's stolen aperture (128 MB on a UHD 630) or an APU's carve-out, and the host memory total would lose that carve-out (it excludes firmware-reserved memory), so neither is used as the ceiling. Without a `Shared Usage` sample the row publishes no used figure rather than the dedicated half alone. A startup row the OS could not classify takes the integrated classification from a later re-detection; a later pass that cannot say does not undo it.

## Unified memory rows

Several devices in this inventory have no memory of their own: an Intel or AMD integrated GPU, an Arm Mali GPU, an RKNPU, an Apple Silicon GPU and an NVIDIA UMA part such as DGX Spark all allocate out of a pool they share with the CPU. Those rows carry `memory_pool:"unified"`, which says one thing only — the `vram_bytes` ceiling is shared with the host and must not be presented as dedicated VRAM. It deliberately says nothing about usage, because whether a unified row can also report `vram_used_bytes` depends on whether anything measures what *that device* is holding. An AMD APU, an Apple Silicon GPU and an NVIDIA UMA part can (amdgpu's `mem_info_vram_used + mem_info_gtt_used`; IOAccelerator's `Alloc system memory`; and, on a part with one physical pool and one allocator — which is why `nvidia-smi` answers `[N/A]` for `memory.total` there — the `/proc/meminfo` figure itself), so those rows publish one. An Intel iGPU, a Mali GPU and an RKNPU cannot: no unprivileged driver counter exists, so `vram_used_bytes` is omitted entirely and a client shows the shared ceiling alone. The host's own RAM usage is never substituted for a missing per-device figure. It used to be, on every unified row, and it produced exactly the claim it looked like: an idle Intel UHD Graphics 630 displayed as using 25.5 GB of 66 GB, and an idle Mali-G610 as using 1.1 GB of 8 GB — in both cases the whole box's memory usage wearing the GPU's label.

## Power draw

`power_watts` is what a device is drawing right now, in whole watts, and it appears **only on rows whose hardware meters itself**. Most of this inventory has no meter at all, and the field is simply absent there — never a literal `0`, which a client would render as "this device is drawing no power".

| row / host | source | reported |
| --- | --- | --- |
| NVIDIA GPU, Linux | `nvidia-smi --query-gpu=power.draw`, on the same 1 s dynamic query as utilization and temperature | yes |
| NVIDIA GPU, Windows | the same query on the 5 s `nvidia-smi` poller, joined to the DXGI adapter by PCI address — the same join the temperature uses | yes |
| AMD discrete GPU, Linux | the amdgpu hwmon's `power1_input` (microwatts), the input labelled `PPT` | yes |
| AMD APU, Linux | the same `PPT` input — the SMU's socket average, CPU cores included — published as **`cpu.power_watts`** when no RAPL package counter is readable (the ordinary case), and not on the APU's GPU row | on the CPU row |
| CPU, Windows | `MSR_PKG_ENERGY_STATUS` (0x611) scaled by `MSR_RAPL_POWER_UNIT` (0x606), read by the elevated `nvpair-sensors` helper through PawnIO and delivered as `cpu.package_watts` over its named pipe | yes, when the helper is installed and running |
| CPU, Linux | `/sys/class/powercap/intel-rapl:<N>/energy_uj` (the same class the `amd_rapl` driver registers under on Zen) | **no in practice** — see below; an AMD APU host reports its package through the row above |
| CPU, macOS | no driverless source | no |
| Intel integrated GPU | i915/xe expose no power attribute to an unprivileged reader | no |
| Arm Mali GPU, RKNPU | no meter in the driver | no |
| Google Coral Edge TPU | the gasket/apex driver keeps no power attribute | no |
| Hailo-8L | HailoRT's `measure-power` is unsupported on that module | no |
| Apple Silicon GPU | IOAccelerator publishes no power figure | no |

**Every power figure is a derivative where the source is a counter.** `energy_uj` and `MSR_PKG_ENERGY_STATUS` are running totals that roll over, so watts are Δenergy / Δt between two ticks. The first tick after a start produces no figure at all, and a delta that can only be a counter re-base (a driver reload, a resume from sleep) is discarded and the baseline dropped, rather than published as the five-digit number the arithmetic would otherwise give. `nvidia-smi` and the amdgpu hwmon report instantaneous power directly and need none of that.

**The Linux CPU ceiling.** Since Linux 5.10 `energy_uj` is mode `0400`, owned by root: the kernel restricted it because the counter is a side channel — power traces recovered AES keys and broke KASLR (CVE-2020-8694) — and no unprivileged interface replaced it. This service runs as the desktop user, so on an ordinary host the read fails with `EACCES`, one `INFO` line at startup names the file and the reason, and `cpu.power_watts` is omitted for the life of the process. Measured on both a Intel + NVIDIA host and an AMD Zen host: the zone is present and reads `package-0`, `max_energy_range_uj` is world-readable, and `energy_uj` is `-r--------`.

The one exception is an AMD APU. Its amdgpu hwmon `PPT` input is world-readable and is the SMU's average socket power — the same quantity as the RAPL package domain (on a Barcelo APU the two agreed within a watt over the same windows) — so a host with an APU and no readable RAPL counter publishes that figure as `cpu.power_watts`. It used to appear as the APU's GPU `power_watts` instead, which left the CPU row blank and would have shown a CPU-bound load as GPU draw. The APU's GPU row now carries no watts, as an Intel integrated GPU row carries none: the SMU's `gpu_metrics` table splits the socket into rails, but on these parts the graphics rail is shared with the CPU cores and the per-core power unit is uncalibrated, so the GPU's own share would be a guess.

This is a documented limit, not a gap to route around. Making it readable would take a setuid helper, a second elevated service, or a boot-time `chmod` of a file the kernel deliberately locked, and a single wattage figure does not justify any of them — the Windows reading exists because an elevated helper *already had to exist* for the package temperature, not because power was worth elevating for.


## Discovery

The service no longer advertises itself over mDNS. Its parent (the broker) registers the `ni` port with the `nvpair-node-scanner` discovery daemon, which carries it on this node's single `_nvpair-node` record; a peer's daemon then fetches `/v1/node-info` over plain HTTP to enrich the node. Transport is derived from the shared per-service policy (`nvpair-shared/noderec`): node-info is served plain on the broker path. The standalone BYO-TLS / `--cluster-dir` mTLS serving above is retained for running node-info on its own, but is unused under the broker.

## Platform Notes

- **Windows** (first-class): GPU inventory comes from DXGI (vendor-agnostic, includes VRAM). Dynamic CPU / VRAM-used / utilization / memory-used numbers come from a persistent PDH query plus `GlobalMemoryStatusEx`. GPU temperature, power draw and NVML utilization (merged with PDH's by taking the higher) come from `nvidia-smi` on a 2 s poller, joined to each DXGI adapter through the display driver's PCI address (`D3DKMTQueryAdapterInfo` / `KMTQAITYPE_ADAPTERADDRESS`), so two identical cards never swap readings; a host without `nvidia-smi` reports none. The CPU package temperature is polled every 5 s from the `nvpair-sensors` service's named pipe (`nvpair-shared/hostsensors`): the sensor is a model-specific register that only the elevated helper can read through PawnIO. A report older than 30 s is dropped, an absent helper is retried every 30 s, and in both cases `cpu.temperature_celsius` is omitted.
- **Linux** (first-class): NVIDIA GPU inventory, dedicated VRAM usage, utilization and temperature come from `nvidia-smi`; CPU and system-memory usage come from `/proc`, the CPU package temperature from hwmon (`coretemp` / `k10temp` / `zenpower` / `cpu_thermal`, else the `x86_pkg_temp` thermal zone). NVIDIA unified-memory GPUs use the `/proc/meminfo` system-memory snapshot even when dynamic `nvidia-smi` collection is unavailable — they are the one row class where that substitution is correct, and no other detector makes it (see [Unified memory rows](#unified-memory-rows)). AMD adapters are read from amdgpu's sysfs nodes (see [Linux AMD (amdgpu sysfs)](#linux-amd-amdgpu-sysfs) below) with full capacity, usage, utilization and temperature, and Intel adapters from i915/xe's sysfs nodes (see [Linux Intel (i915 / xe sysfs)](#linux-intel-i915--xe-sysfs) below) with a name and a shared-pool capacity but deliberately no utilization and no memory usage. Every vendor detector runs and their rows are concatenated in the order NVIDIA → Rockchip → AMD → Intel, so a host with a discrete card and an integrated one lists both; `ghw` supplies names only when no vendor detector found anything at all. Inference accelerators behind the gasket/apex driver (Google Coral Edge TPU, `/sys/class/apex/*`) are listed with `kind:"npu"`; the driver keeps no busy counter, so `utilization_percent` is the fraction of 100 ms sub-intervals in the last second in which the device's `interrupt_counts` moved (the same "percent of time working" definition as `nvidia-smi`'s `utilization.gpu`, at coarser resolution), and `temperature_celsius` comes from its `temp` attribute.
- **macOS**: CPU and system-memory usage come from Mach through gopsutil's purego bindings. GPU identity, mapped memory, and utilization come from the built-in, unprivileged `/usr/sbin/ioreg` command's `IOAccelerator` `PerformanceStatistics`; no sudo or private framework binding is required. Apple Silicon is supported directly. Intel/AMD fields are best-effort when their drivers expose the same dedicated-memory counters. The performance keys are undocumented and may change across macOS releases; a missing or changed key leaves only that metric out and does not stop CPU or memory collection.
- **Other platforms**: GPU names come from `ghw`; VRAM and dynamic stats are not reported.

### Windows inventory: stale DirectX registry at boot

DXGI enumeration is filtered against the `AdapterLuid` values under `HKLM\SOFTWARE\Microsoft\DirectX`, which is how an RDP phantom clone of a card (same name, a second LUID) is kept out of the inventory. Windows reassigns adapter LUIDs on every boot but only rewrites those registry keys about a minute into the session, so a service that enumerates before that is matching this boot's LUIDs against the previous boot's: nothing matches, and the gate would drop every real adapter. That is not a state a machine with GPUs can be in, so an empty result is treated as proof the registry is stale — every adapter is kept and one warning is logged with the counts. When the gate keeps at least one adapter it still filters exactly as before.

Detection also runs again after startup, on its own goroutine, never on the 1 s stats tick. While the inventory is empty it retries **every 10 s** until adapters appear; once at least one is known it re-detects **every 60 s** and republishes only when the set of adapters changed (a card hot-plugged after startup, or a clone a remote session brought in). Recovered adapters carry the same LUID key the PDH counters and the `nvidia-smi` temperature join use, so their VRAM, utilization and temperature fill in on the next tick; the temperature poller resolves an adapter address it has not seen before on demand. A node that came up with an empty GPU list therefore repairs itself instead of needing a restart.

A LUID is not fixed for the life of a boot: installing, updating or restarting a card's display driver while the service runs gives the same card a new LUID at the same PCI address (measured on an RTX 5060 whose driver was re-added two minutes after the service started; before this was handled the card was listed twice, the startup row with its temperature and power and a second row with its VRAM used). Every Windows row therefore also carries its PCI address as a hardware identity, read once per detection through `D3DKMTQueryAdapterInfo`. When a re-detection reports an unknown LUID at the address of a startup row whose own LUID is no longer detected, that row moves to the new LUID and keeps its place; a second adapter at an address whose startup LUID is still live (a remote-session clone) is still listed separately. A detection that gains a LUID also makes the temperature poller re-resolve every address, so temperature and power follow the card to its new key; a detection that only loses one — the ~14 s before the DirectX registry lists the new LUID — leaves the join alone so the existing row keeps its reading. If a card's address could not be read at startup, the first detection that reads it republishes the inventory and the pairing is remembered for the life of the process, so a later reissue still finds the startup row. One path is not covered: if the address is unreadable at startup and the LUID is reissued before any detection reads it, no pairing exists for the old LUID and the card is listed twice until the service restarts. A moved row takes the name and memory of the adapter now at that address. Convergence after a driver reinstall is one re-detection (60 s) plus one temperature poll (2 s).

### GPU marketing names

Intel and AMD integrated GPUs are named from one table, shared by every platform: `nvpair-shared/gpunames` maps a PCI device id to `<marketing name> (<codename>, <architecture>)`, and Linux and Windows both look up the same id there. Windows used to publish whatever the display driver’s INF said, which is vague and inconsistent for an iGPU — a Tiger Lake laptop enumerated as `Intel(R) UHD Graphics` with no model and no generation while a Coffee Lake desktop enumerated as `Intel(R) UHD Graphics 630`, and neither string named the architecture an operator actually schedules against. DXGI hands us the device id in the same struct as that description, so those rows now read `Intel UHD Graphics (Tiger Lake, Xe-LP)` and `Intel UHD Graphics 630 (Coffee Lake, Gen 9.5)` — identical to what the sysfs inventories publish for the same silicon on Linux. The change is strictly additive: an id the table does not carry keeps DXGI’s description unchanged, and NVIDIA adapters are never touched, because `nvidia-smi` and the NVIDIA INF already name a card precisely. Every id in the table comes from the kernel’s own id header (`include/drm/intel/pciids.h`, historically `i915_pciids.h` / `xe_pciids.h`) or the amdgpu PCI table, with the marketing name from the PCI ID Repository’s `pci.ids` and libdrm’s `amdgpu.ids`; where the two disagree the kernel macro decides the codename and `pci.ids` decides the marketing name. Nothing about the wire contract changes — these strings only ever land in `gpus[].name`, which has always been free-form.

### Linux AMD (amdgpu sysfs)

AMD ships no `nvidia-smi` equivalent in a default install, so a Radeon host used to fall through to `ghw` and publish the PCI database's codename and nothing else (`{"name":"Barcelo"}`). The amdgpu driver already exposes everything needed under `/sys/class/drm/card<N>/device/`, world-readable, so the inventory and the per-tick sample are plain file reads — no daemon, no root, no cgo, no new dependency.

What is read, per card:

| attribute | use |
| --- | --- |
| `vendor`, `device` | vendor `0x1002` selects the card; the device id picks the marketing name |
| `uevent` (`PCI_SLOT_NAME`) | join-key fallback when the `device` symlink cannot be resolved |
| `mem_info_vram_total` | dedicated VRAM, or an APU's firmware carve-out |
| `mem_info_gtt_total` | the GTT aperture — system memory the GPU may map |
| `mem_info_vram_used`, `mem_info_gtt_used` | `vram_used_bytes`, sampled every collector tick |
| `gpu_busy_percent` | `utilization_percent`, already the 0..100 figure `nvidia-smi`'s `utilization.gpu` reports |
| `hwmon/hwmon<N>/temp*_input` + `temp*_label` | `temperature_celsius`, in millidegrees, from the sensor labelled `edge` |

Only `card<N>` directories are enumerated. The DRM class also carries one `card<N>-<CONNECTOR>` entry per output (`card1-DP-1`, `card1-HDMI-A-1`, …) and `renderD<N>` nodes, all pointing back at the same device; listing them would publish the same GPU several times.

**Unified-pool capacity rule.** On an APU `mem_info_vram_total` is only the carve-out the firmware reserved — 512 MiB on a Ryzen 5 5625U — while the real ceiling for a model is that carve-out plus the GTT aperture. So:

- **APU** (a device id in the built-in table, or `mem_info_vram_total` under 1 GiB alongside a `mem_info_gtt_total`): `vram_bytes = mem_info_vram_total + mem_info_gtt_total`, and `vram_used_bytes = mem_info_vram_used + mem_info_gtt_used`.
- **Discrete card**: `vram_bytes = mem_info_vram_total` and `vram_used_bytes = mem_info_vram_used` — a discrete card also exposes a GTT aperture, and counting it would inflate a 16 GiB card to 24 GiB.

Capacity and usage always come from the same side of that rule, so `vram_used_bytes` can never exceed `vram_bytes`. An APU row carries `memory_pool:"unified"` because that capacity is shared with the CPU, but it is *not* the unified-memory NVIDIA case: that one substitutes whole-system RAM usage for a GPU whose driver cannot report its own, while amdgpu reports its own usage precisely and is always believed. The APU is the shared-pool row that keeps a used figure for exactly that reason — see [Unified memory rows](#unified-memory-rows).

**Join key.** Each row's internal `statsKey` is `amd:<pci address>` — `amd:0000:04:00.0` — taken from resolving the `device` symlink into `/sys/bus/pci/devices`, or from `uevent`'s `PCI_SLOT_NAME`, or, as a last resort, the DRM node name (`amd:card1`). The key never reaches the wire; it exists so the static row and the collector's sample meet. The `amd:<pci>` spelling matches the upstream amdgpu inventory work, so both implementations key identically.

**Names.** The shared table (see [GPU marketing names](#gpu-marketing-names)) maps the device id to `<marketing name> (<codename>, <architecture>)`, because a bare codename is what the PCI database gives and it answers nothing: `Barcelo` names neither the vendor nor the generation, and AMD has reused "AMD Radeon Graphics" across five architectures. So `0x15e7` publishes `AMD Radeon Vega Graphics (Barcelo, GCN 5.1)`.

| device id | published name |
| --- | --- |
| `0x15dd` | `AMD Radeon Vega Graphics (Raven Ridge, GCN 5)` |
| `0x15d8` | `AMD Radeon Vega Graphics (Picasso, GCN 5)` |
| `0x1636` | `AMD Radeon Vega Graphics (Renoir, GCN 5.1)` |
| `0x164c` | `AMD Radeon Vega Graphics (Lucienne, GCN 5.1)` |
| `0x1638` | `AMD Radeon Vega Graphics (Cezanne, GCN 5.1)` |
| `0x15e7` | `AMD Radeon Vega Graphics (Barcelo, GCN 5.1)` |
| `0x164e` | `AMD Radeon Graphics (Raphael, RDNA 2)` |
| `0x1681` | `AMD Radeon 680M (Rembrandt, RDNA 2)` |
| `0x15bf` | `AMD Radeon 780M (Phoenix, RDNA 3)` |
| `0x15c8` | `AMD Radeon 740M (Phoenix2, RDNA 3)` |
| `0x1900` | `AMD Radeon 780M (Hawk Point, RDNA 3)` |
| `0x150e` | `AMD Radeon 890M (Strix Point, RDNA 3.5)` |
| `0x1586` | `AMD Radeon 8060S (Strix Halo, RDNA 3.5)` |
| `0x1114` | `AMD Radeon 860M (Krackan Point, RDNA 3.5)` |

Every id was checked against the kernel's amdgpu PCI table (`drivers/gpu/drm/amd/amdgpu/amdgpu_drv.c`), the PCI ID Repository's `pci.ids`, and libdrm's `data/amdgpu.ids`. Three are easy to transpose and are worth stating plainly: **`0x1900` is Hawk Point, not Strix Point**; **`0x150e` is Strix Point, not Strix Halo**; **`0x1586` is Strix Halo**.

**No compute-unit count is ever printed**, and that is the reason the old `(Vega 7, Barcelo)` string is gone. The CU count is not derivable from the device id: `0x15e7` alone ships as Vega 6, Vega 7 and Vega 8, separated only by the PCI *revision* id, so the published figure was wrong on most SKUs.

**Unlisted ids.** A part released after the table was written still gets a row, named `AMD Radeon Graphics (device 0x<id>)` — never a bare codename, never a dropped row. When the driver exposes its IP-discovery tree at `/sys/class/drm/card<N>/device/ip_discovery/die/0/GC/0/{major,minor}`, the Graphics Core IP version is mapped to an architecture family and the row becomes `AMD Radeon Graphics (device 0x<id>, RDNA 3.5)`: `9.0`–`9.2` → `GCN 5`, `9.3` → `GCN 5.1` (gfx90c, measured on the Barcelo host), `10.1` → `RDNA`, `10.3` → `RDNA 2`, `11.0` → `RDNA 3`, `11.5` → `RDNA 3.5`, `12.0` → `RDNA 4`. GC `9.4.x`/`9.5.x` are deliberately unmapped — those are the data-center CDNA parts, not GCN 5.x — and any unmapped version leaves the architecture out rather than inventing one.

**Ceilings and caveats.**

- Requires the `amdgpu` kernel driver. The older `radeon` driver and pre-Vega parts expose none of these attributes; such a card keeps the `ghw` name-only behavior.
- `gpu_busy_percent` is the driver's own coarse busy figure, not per-engine and not per-process, and a few ASICs do not export it at all. A missing or out-of-range value is dropped rather than published as `0`, so a driver that cannot answer never looks idle — and never marks the node's telemetry fresh.
- The GTT half of an APU's capacity is an aperture the GPU may map, not memory reserved for it: the CPU is using most of it, and `memory.total_bytes` counts the same RAM. Treat an APU's `vram_bytes` as a ceiling, not as free memory.
- `temperature_celsius` is the **edge** sensor. The same hwmon usually also exposes `junction` (hotspot) and `mem`, which read considerably hotter; edge is the closest analogue to `nvidia-smi`'s `temperature.gpu`, so rows stay comparable across vendors. With no labels at all, `temp1` is used.
- Power (`power1_input`) and clocks are exposed by the same hwmon but are not part of this wire contract.
- A host with no AMD card costs one `os.ReadDir` of `/sys/class/drm` per tick and logs nothing; a host without `/sys/class/drm` at all logs one `Debug` line for the process lifetime.

- **Windows** (first-class): GPU inventory comes from DXGI (vendor-agnostic, includes VRAM). Dynamic CPU / VRAM-used / utilization / memory-used numbers come from a persistent PDH query plus `GlobalMemoryStatusEx`. GPU temperature, power draw and NVML utilization (merged with PDH's by taking the higher) come from `nvidia-smi` on a 2 s poller, joined to each DXGI adapter through the display driver's PCI address (`D3DKMTQueryAdapterInfo` / `KMTQAITYPE_ADAPTERADDRESS`), so two identical cards never swap readings; a host without `nvidia-smi` reports none. The CPU package temperature is polled every 5 s from the `nvpair-sensors` service's named pipe (`nvpair-shared/hostsensors`): the sensor is a model-specific register that only the elevated helper can read through PawnIO. A report older than 30 s is dropped, an absent helper is retried every 30 s, and in both cases `cpu.temperature_celsius` is omitted. Hailo inference accelerators are listed with `kind:"npu"` and a temperature; see **Windows Hailo (HailoRT)** below.
- **Linux** (first-class): NVIDIA GPU inventory, dedicated VRAM usage, utilization and temperature come from `nvidia-smi`; CPU and system-memory usage come from `/proc`, the CPU package temperature from hwmon (`coretemp` / `k10temp` / `zenpower` / `cpu_thermal`, else the `x86_pkg_temp` thermal zone). NVIDIA unified-memory GPUs use the `/proc/meminfo` system-memory snapshot even when dynamic `nvidia-smi` collection is unavailable — they are the one row class where that substitution is correct, and no other detector makes it (see [Unified memory rows](#unified-memory-rows)). AMD adapters are read from amdgpu's sysfs nodes and Intel adapters from i915/xe's (see [Linux AMD (amdgpu sysfs)](#linux-amd-amdgpu-sysfs) and [Linux Intel (i915 / xe sysfs)](#linux-intel-i915--xe-sysfs) below); every vendor detector runs and their rows are concatenated, so a host with a discrete card and an integrated one lists both, and `ghw` supplies names only when no vendor detector found anything at all. Inference accelerators behind the gasket/apex driver (Google Coral Edge TPU, `/sys/class/apex/*`) are listed with `kind:"npu"`; the driver keeps no busy counter, so `utilization_percent` is the fraction of 100 ms sub-intervals in the last second in which the device's `interrupt_counts` moved (the same "percent of time working" definition as `nvidia-smi`'s `utilization.gpu`, at coarser resolution), and `temperature_celsius` comes from its `temp` attribute.
- **macOS**: CPU and system-memory usage come from Mach through gopsutil's purego bindings. GPU identity, mapped memory, and utilization come from the built-in, unprivileged `/usr/sbin/ioreg` command's `IOAccelerator` `PerformanceStatistics`; no sudo or private framework binding is required. Apple Silicon is supported directly. Intel/AMD fields are best-effort when their drivers expose the same dedicated-memory counters. The performance keys are undocumented and may change across macOS releases; a missing or changed key leaves only that metric out and does not stop CPU or memory collection.
- **Other platforms**: GPU names come from `ghw`; VRAM and dynamic stats are not reported.

### Linux Intel (i915 / xe sysfs)

Intel adapters had no detector at all, and the old chain made that worse: it returned at the first source that produced anything, so a node with a discrete card and an Intel iGPU published only the discrete card. Measured case — an NVIDIA A2 on `card0` and a CoffeeLake-S GT2 (`8086:3e98`, driver `i915`) on `card1` — listed one GPU where the machine has two. The detectors are now composed, in the order NVIDIA → Rockchip → AMD → Intel, and `ghw` runs only when every one of them came back empty.

What is read, per card, all world-readable and with no daemon, no root and no cgo:

| attribute | use |
| --- | --- |
| `vendor` | `0x8086` selects the card |
| `driver` → `…/drivers/i915` | the symlink is resolved and the driver must be `i915` or `xe`; `uevent`'s `DRIVER=` is the fallback |
| `device` | the device id picks the marketing name |
| `uevent` (`PCI_SLOT_NAME`) | join-key fallback when the `device` symlink cannot be resolved |
| `mem_info_vram_total` | dedicated VRAM, on a discrete card that publishes it |
| `hwmon/hwmon<N>/temp1_input` | `temperature_celsius`, in millidegrees, on a discrete Arc card |

Only `card<N>` directories are enumerated, for the same reason as the AMD path — and it matters more here, because the Intel iGPU is usually the card that owns every display connector on the box.

**Two filters, both required.** The vendor id keeps the NVIDIA and AMD cards on the same host out of this inventory; they have their own detectors with real telemetry. The bound driver keeps out an Intel GPU handed to a guest through `vfio-pci`, whose attributes this host cannot read and must not claim.

**Memory.** An integrated Intel GPU has no dedicated VRAM — it allocates out of system DRAM — so its row carries `memory_pool:"unified"` with the system-memory total as `vram_bytes`, exactly like the Mali rows below. Treat that as a ceiling the CPU is also spending from, not as memory reserved for the GPU. The row reports **no** `vram_used_bytes`: i915 and xe publish no unprivileged per-device allocation counter, and `/proc/meminfo` describes every process on the host rather than this GPU (substituting it is what displayed an idle UHD Graphics 630 as "25.5 GB / 66 GB"). A discrete card reports its own `mem_info_vram_total` and no `memory_pool`; when the driver does not publish the attribute (i915 does not for an iGPU, and not on every kernel for a discrete card) the capacity stays unknown and `omitempty` drops it.

**Names.** `0x3e98` → `Intel UHD Graphics 630 (Coffee Lake, Gen 9.5)`, in the same `<marketing name> (<codename>, <architecture>)` shape the AMD table uses, from the shared table described under [GPU marketing names](#gpu-marketing-names). Coffee, Whiskey, Amber and Comet Lake (Gen 9.5), Rocket, Tiger, Alder and Raptor Lake plus DG1 (Xe-LP), Alchemist and ATS-M (Xe-HPG), Meteor and Arrow Lake (Xe-LPG), Lunar Lake (Xe2) and Battlemage (Xe2-HPG) are listed; an unlisted id publishes `Intel Graphics (device 0x<id>)`. Every id was checked against the kernel's `include/drm/intel/pciids.h` and the PCI ID Repository's `pci.ids` — `0x9a60`/`0x9a68`/`0x9a70` are Tiger Lake **GT1**, sold as UHD Graphics, so they are named that and not Iris Xe; Iris Xe on Tiger Lake is the GT2 part (`0x9a40`, `0x9a49`), listed separately — except `0x9a78`, which `pci.ids` names `UHD Graphics G4` although the kernel groups it with the GT2 ids, so it keeps the name its source gives it.

**Join key.** `intel:<pci address>` — `intel:0000:00:02.0` — built by the same helper the `amd:` keys use, with the same `PCI_SLOT_NAME` and DRM-node fallbacks.

**Ceilings and caveats.**

- **No `utilization_percent`, ever.** i915 keeps its engine-busy counters in a PMU reached through `perf_event_open`, which `perf_event_paranoid` gates behind root; there is no unprivileged sysfs attribute equivalent to amdgpu's `gpu_busy_percent`. The field is therefore absent rather than `0`, and the row carries `utilization_unavailable:true`, because "idle" and "we cannot tell" must not render the same (an idle `0` is absent too, so the absence alone cannot say which). This is a driver limitation, not an unfinished feature.
- **Not inference-ready when integrated.** An integrated Intel GPU row carries `inference_ready:false`: no engine build PAIR runs on Linux was found with a backend that drives it. A discrete Arc card makes no such claim either way. The same part on **Windows** deliberately carries no `inference_ready` at all: whether the Windows engine builds drive it has not been established, and an absent flag makes no claim either way rather than asserting one.
- **No temperature on an integrated GPU.** The only sensor near it is the CPU package sensor, which is already published as `cpu.temperature_celsius`; repeating it as the GPU's would be a reading from different silicon. A discrete Arc card has its own hwmon and that one is read, and because it is a temperature and not a utilization sample it never marks the node's GPU telemetry fresh.
- An Intel row's presence therefore does not make `telemetryValid` true on an Intel-only host. Nothing about it is a live GPU sample.
- Requires the `i915` or `xe` kernel driver. An adapter on neither keeps the `ghw` name-only behavior, and only when no other vendor detector found anything.

**Live check.** Unit tests cover the parsing against a fake DRM tree; one live test, gated on `NVPAIR_LIVE_INTEL=1`, runs the real detectors on the hardware and asserts both halves of the fix — the Intel row exists, and the NVIDIA card is still first.

```
GOOS=linux go test -c -o intel.test
NVPAIR_LIVE_INTEL=1 ./intel.test -test.v -test.run Live
```

- **Windows** (first-class): GPU inventory comes from DXGI (vendor-agnostic, includes VRAM). Dynamic CPU / VRAM-used / utilization / memory-used numbers come from a persistent PDH query plus `GlobalMemoryStatusEx`. GPU temperature, power draw and NVML utilization (merged with PDH's by taking the higher) come from `nvidia-smi` on a 2 s poller, joined to each DXGI adapter through the display driver's PCI address (`D3DKMTQueryAdapterInfo` / `KMTQAITYPE_ADAPTERADDRESS`), so two identical cards never swap readings; a host without `nvidia-smi` reports none. The CPU package temperature is polled every 5 s from the `nvpair-sensors` service's named pipe (`nvpair-shared/hostsensors`): the sensor is a model-specific register that only the elevated helper can read through PawnIO. A report older than 30 s is dropped, an absent helper is retried every 30 s, and in both cases `cpu.temperature_celsius` is omitted. Hailo inference accelerators are listed with `kind:"npu"` and a temperature; see **Windows Hailo (HailoRT)** below.
- **Linux** (first-class): NVIDIA GPU inventory, dedicated VRAM usage, utilization and temperature come from `nvidia-smi`; CPU and system-memory usage come from `/proc`, the CPU package temperature from hwmon (`coretemp` / `k10temp` / `zenpower` / `cpu_thermal`, else the `x86_pkg_temp` thermal zone). NVIDIA unified-memory GPUs use the `/proc/meminfo` system-memory snapshot even when dynamic `nvidia-smi` collection is unavailable — they are the one row class where that substitution is correct, and no other detector makes it (see [Unified memory rows](#unified-memory-rows)). Non-NVIDIA adapters fall back to names from `ghw` without dynamic GPU stats. Inference accelerators behind the gasket/apex driver (Google Coral Edge TPU, `/sys/class/apex/*`) are listed with `kind:"npu"`; the driver keeps no busy counter, so `utilization_percent` is the fraction of 100 ms sub-intervals in the last second in which the device's `interrupt_counts` moved (the same "percent of time working" definition as `nvidia-smi`'s `utilization.gpu`, at coarser resolution), and `temperature_celsius` comes from its `temp` attribute.
- **macOS**: CPU and system-memory usage come from Mach through gopsutil's purego bindings. GPU identity, mapped memory, and utilization come from the built-in, unprivileged `/usr/sbin/ioreg` command's `IOAccelerator` `PerformanceStatistics`; no sudo or private framework binding is required. Apple Silicon is supported directly. Intel/AMD fields are best-effort when their drivers expose the same dedicated-memory counters. The performance keys are undocumented and may change across macOS releases; a missing or changed key leaves only that metric out and does not stop CPU or memory collection.
- **Other platforms**: GPU names come from `ghw`; VRAM and dynamic stats are not reported.

### Windows Hailo (HailoRT)

A Hailo M.2 module is not a display adapter, so DXGI never sees it. `nvpair-node-info` lists it in the same `GPUs` inventory with `kind:"npu"`, alongside the Edge TPU rows Linux produces, by calling HailoRT's C API (`libhailort.dll`) directly through `LazyDLL` — no cgo and no new dependency.

**Where the library is looked for**, in order:

1. `%HAILORT_DIR%\libhailort.dll` and `%HAILORT_DIR%\bin\libhailort.dll`
2. `%HAILORT_ROOT%\libhailort.dll` and `%HAILORT_ROOT%\bin\libhailort.dll`
3. `%ProgramFiles%\HailoRT\bin\libhailort.dll` (the installer's default)
4. `libhailort.dll` through the loader's own search path

**What is reported.** `hailo_scan_devices` yields one row per module, keyed by its PCIe BDF. The row's name comes from `hailo_identify`'s `device_architecture` (`HAILO8L` → "Hailo-8L AI Accelerator", and likewise for Hailo-8 / 15H / 15L / 15M / 10H); an architecture this build does not know is named "Hailo AI Accelerator" rather than dropped. A sampler goroutine per device then holds one open handle and reads `hailo_get_chip_temperature` every 5 s, publishing the hotter of the two on-die sensors (TS0/TS1) as `temperature_celsius`, rounded. A reading whose `sample_count` is 0 is discarded. Three consecutive failed reads drop the handle so the next tick reopens it — a handle does not survive a driver restart or a surprise removal, and every read on a dead one fails forever.

**Ceilings on this platform. These are limits of HailoRT 4.24 on Windows, not gaps to work around:**

- **No busy counter.** HailoRT exposes none and its monitor mode is unsupported on Windows (`hailortcli monitor` refuses), so node-info cannot measure utilization from the device. The figure comes from the process that runs inference on it instead — see [Utilization from the activity file](#utilization-from-the-activity-file). Without that file the row carries **no** `utilization_percent`, never a literal `0`, which would read as "idle", and carries `utilization_unavailable:true` so a client can tell that absence from an idle reading.
- **No power.** Power measurement is unsupported on the M.2 Hailo-8L module, so `hailo_power_measurement` is not called.
- So the row is **presence + name + temperature**, plus utilization while an inference process reports it.

#### Utilization from the activity file

The process that runs inference on the module tracks how long it has had at least one device call in flight and publishes that as a small JSON file. The sampler reads it on its own 5 s tick, right after the temperature, and the two sources are independent: a device the sampler cannot open still gets its utilization, and a writer that is not running costs the temperature nothing.

**Where.** `%NVPAIR_ACCEL_ACTIVITY_DIR%\hailo.json` when that variable is set, else `%ProgramData%\nvpair\accel-activity\hailo.json`.

**Schema 1** (the writer implements exactly this; node-info rejects anything else):

```json
{"schema":1,"device":"hailo-8l","pid":1234,"started_ms":1700000000000,"updated_ms":1700000005000,"busy_ms":2500,"inflight":1}
```

| field | meaning |
| --- | --- |
| `pid` | the writer's process id |
| `started_ms` | epoch ms at which the writer's tracker started |
| `updated_ms` | epoch ms of this write |
| `busy_ms` | cumulative ms with at least one device call in flight, including the in-flight portion up to `updated_ms` |
| `inflight` | device calls in flight at `updated_ms` |

The file is UTF-8 (a byte-order mark is tolerated), replaced by temp-and-rename about every 500 ms while the writer lives, with a last write carrying `inflight:0` when it exits.

**Reader rules.**

- **Utilization** = (`busy_ms`₂ − `busy_ms`₁) / (`updated_ms`₂ − `updated_ms`₁) × 100 between two successive *distinct* samples of the same writer, measured entirely on the writer's own clock; clamped to 0–100 and rounded to a whole percent like every other row. The first sample of a writer is only a baseline, so a newly found writer shows "unavailable" for one tick before its first figure.
- **Same writer** means the same `pid` and `started_ms`. A change in either — a restarted writer, whose `busy_ms` starts over — resets the baseline, as do counters that go backwards within one writer.
- **Same `updated_ms` as last tick** is no new data: the last figure is held while the sample is fresh.
- **Fresh** = `updated_ms` within 3 s of node-info's clock (six missed writes). A fresh file publishes `utilization_percent` and no `utilization_unavailable` — including a measured `0`, which is "idle".
- **Stale, writer gone** — the pid does not exist, has exited, or belongs to a process created after `started_ms` (a reused pid): the inference process has ended, so the device is idle and the row publishes `0`.
- **Stale, writer alive** — the writer is running but not reporting: `utilization_unavailable:true`. (A process node-info may not inspect is treated as alive, which is the answer that publishes no figure.)
- **Absent, unreadable, not JSON, wrong `schema` or `device`, or a missing field**: `utilization_unavailable:true`, exactly as without a writer.
- The file names a device type, not a module, so with **more than one module** fitted it cannot be attributed and every Hailo row stays `utilization_unavailable`. The PnP presence rows below have no sampler and are always `utilization_unavailable`.
- node-info logs one line per state change (absent, invalid, pending, fresh, writer gone, writer silent), never one per tick.

**A measured idle 0 on the wire.** `utilization_percent` is `omitempty`, so a fresh idle writer's `0` is absent from the row — and so is `utilization_unavailable`. That combination is the historical "idle" shape: the desktop reads an absent `utilization_percent` as `0` and renders "0%", and renders "—" only when `utilization_unavailable` is `true`.

**How the file is opened.** Each tick opens it with `FILE_SHARE_READ | FILE_SHARE_WRITE | FILE_SHARE_DELETE`, reads it, and closes it at once. Measured on Windows 11 / NTFS: a POSIX-semantics rename (`FILE_RENAME_FLAG_POSIX_SEMANTICS`) replaces the file while a reader holds it only if the reader shares delete, which `os.Open` does not; a classic `MoveFileEx(MOVEFILE_REPLACE_EXISTING)` replace (Python's `os.replace`) fails with "access denied" while *any* handle is open, whatever its share mode. The reader therefore holds its handle for one small read every 5 s, and a writer that replaces with `MoveFileEx` should treat a failed replace as transient and write again on its next tick.

**Without HailoRT installed**, a fitted module is still listed from the PnP enumerator (`HKLM\SYSTEM\CurrentControlSet\Enum\PCI\VEN_1E60&DEV_*`), named from its PCI device id, with no temperature. That branch also retains an entry for a module that has since been removed, so the library scan — which talks to the hardware — is always preferred and the registry is read only when it is unavailable. A host with neither the library nor the device logs one Debug line and reports no accelerator, exactly as before.

Like every accelerator row, a Hailo device never contributes to `telemetryValid` / `msSince`, is skipped by `noderec.MaxGPUUtilization`, and carries `inference_ready:false`: it cannot run the engines PAIR schedules.

A live check against real hardware ships with the tests and is skipped unless `NVPAIR_LIVE_HAILO=1` is set:

```
go test -c -o hailo_windows.test.exe
NVPAIR_LIVE_HAILO=1 hailo_windows.test.exe -test.run TestLiveHailoAccelerator -test.v

- **Linux** (first-class): NVIDIA GPU inventory, dedicated VRAM usage, utilization and temperature come from `nvidia-smi`; CPU and system-memory usage come from `/proc`, the CPU package temperature from hwmon (`coretemp` / `k10temp` / `zenpower` / `cpu_thermal`, else the first present of the `x86_pkg_temp`, `cpu-thermal`, `soc-thermal` and `cpu_thermal` thermal zones). NVIDIA unified-memory GPUs use the `/proc/meminfo` system-memory snapshot even when dynamic `nvidia-smi` collection is unavailable — they are the one row class where that substitution is correct, and no other detector makes it (see [Unified memory rows](#unified-memory-rows)). AMD adapters are read from amdgpu's sysfs nodes and Intel adapters from i915/xe's (see [Linux AMD (amdgpu sysfs)](#linux-amd-amdgpu-sysfs) and [Linux Intel (i915 / xe sysfs)](#linux-intel-i915--xe-sysfs) below); every vendor detector runs and their rows are concatenated, so a host with a discrete card and an integrated one lists both, and `ghw` supplies names only when no vendor detector found anything at all. Inference accelerators behind the gasket/apex driver (Google Coral Edge TPU, `/sys/class/apex/*`) are listed with `kind:"npu"`; the driver keeps no busy counter, so `utilization_percent` is the fraction of 100 ms sub-intervals in the last second in which the device's `interrupt_counts` moved (the same "percent of time working" definition as `nvidia-smi`'s `utilization.gpu`, at coarser resolution), and `temperature_celsius` comes from its `temp` attribute. Arm SoC boards have their own detectors — see *Linux Rockchip* below.
- **macOS**: CPU and system-memory usage come from Mach through gopsutil's purego bindings. GPU identity, mapped memory, and utilization come from the built-in, unprivileged `/usr/sbin/ioreg` command's `IOAccelerator` `PerformanceStatistics`; no sudo or private framework binding is required. Apple Silicon is supported directly. Intel/AMD fields are best-effort when their drivers expose the same dedicated-memory counters. The performance keys are undocumented and may change across macOS releases; a missing or changed key leaves only that metric out and does not stop CPU or memory collection.
- **Other platforms**: GPU names come from `ghw`; VRAM and dynamic stats are not reported.

### Linux Rockchip (Mali via devfreq, RKNPU via debugfs)

Rockchip RK35xx boards (measured on an RK3588S, vendor kernel 6.1) have no `nvidia-smi` and no PCI display adapter, so both Linux GPU detectors come back empty. Their Mali GPU and NPU are platform devices found in sysfs instead, and both are listed in the same `GPUs` inventory. Neither can run PAIR's engines (no engine ships a Mali or RKNPU backend), so both rows carry `inference_ready:false`:

| device | row | `statsKey` | utilization | temperature |
| --- | --- | --- | --- | --- |
| Arm Mali GPU | `"Arm Mali-G610 MP4"` (from `/sys/class/misc/mali0/device/gpuinfo`) | `mali:<devfreq node>` | devfreq `load`, `"<busy%>@<freq>Hz"`; the driver's `utilisation` attribute (0..100) when a kernel exposes no devfreq load | thermal zone `gpu-thermal` |
| RKNPU | `"Rockchip RK3588S NPU (3 cores)"`, `kind:"npu"` (SoC from the board's root device-tree `compatible` — the same token the CPU row is named from — when it is a variant of the family the NPU node's own `compatible` names, since an RK3588S's NPU node says `rockchip,rk3588-rknpu`; core count from the driver) | `rknpu:<devfreq node>` | `/sys/kernel/debug/rknpu/load`, the mean across cores | thermal zone `npu-thermal` |

Both are sampled in their own goroutine once a second and folded into the collector's snapshot, so a wedged driver node cannot delay the 1 s tick. Neither marks `telemetryValid`: PAIR's engines run on neither device, exactly as for a Coral Edge TPU.

Memory is unified on these SoCs — there is no dedicated VRAM — so both rows report total system RAM as `vram_bytes` with `memory_pool:"unified"`, and neither reports `vram_used_bytes`: no Mali or RKNPU driver counter says how much of that pool the device holds, and the board's own memory usage is not an answer to that question (it read as an idle Mali-G610 using 1.1 GB of 8 GB). See [Unified memory rows](#unified-memory-rows).

**The NPU's utilization requires readable debugfs.** The RKNPU devfreq node also publishes a `load`, but it reads a constant `100@…Hz` while the NPU is idle, so it is never used; the driver's real per-core counter is only in debugfs, which the kernel mounts `0700` (root only). Without it the NPU row stays in the inventory with its temperature, `utilization_percent` is omitted and `utilization_unavailable:true` says why, and the service logs one line naming the file and the fix. The same flag marks either row before its first successful read, and after three consecutive failed reads, instead of holding a value nothing is refreshing. To make it readable by the unprivileged service, remount debugfs world-readable at boot (e.g. a small systemd unit ordered before the service):

```sh
mount -o remount,mode=755 /sys/kernel/debug
```

The service never attempts the remount itself, and it reads nothing else from debugfs.

CPU identity on these boards is repaired from the device tree, because `/proc/cpuinfo` gives `ghw` neither a model name nor the full core count: the kernel groups the asymmetric clusters into separate packages, so `ghw` reports one cluster (4) rather than the SoC's 8 cores. `/proc/device-tree/model` and the first `compatible` entry supply the board and the SoC, and the `processor` entries of `/proc/cpuinfo` raise the core count — both only on a host that has a device tree, so an x86 host keeps `ghw`'s physical-core count untouched. The CPU package temperature falls back to the `cpu-thermal`, `soc-thermal` and `cpu_thermal` thermal zones (in that order, after `x86_pkg_temp`) because no hwmon entry on these boards names a CPU driver; the SoC-wide zone is preferred over arbitrarily picking one cluster's.

The shape of a response from an idle board with readable debugfs (an idle 0 % utilization is omitted, as on every row):

```json
{
  "GPUs": [
    {"name": "Arm Mali-G610 MP4", "vram_bytes": 8587837440, "temperature_celsius": 40, "memory_pool": "unified", "inference_ready": false},
    {"name": "Rockchip RK3588S NPU (3 cores)", "vram_bytes": 8587837440, "kind": "npu", "temperature_celsius": 41, "memory_pool": "unified", "inference_ready": false}
  ],
  "cpu": {"name": "Rockchip RK3588S (Orange Pi 5)", "cores": 8, "utilization_percent": 9, "temperature_celsius": 42},
  "memory": {"total_bytes": 8587837440, "used_bytes": 907354112},
  "telemetryValid": false,
  "msSince": 0
}
```

The board-specific path is covered by unit tests against fake sysfs trees plus one live test, gated on `NVPAIR_LIVE_ROCKCHIP=1`, that runs the real detectors and collector on the hardware:

```sh
GOOS=linux GOARCH=arm64 go test -c -o rockchip_arm64.test     # cross-compile
NVPAIR_LIVE_ROCKCHIP=1 ./rockchip_arm64.test -test.v          # on the board
```

## Shutdown

The service shuts down on:

- stdin EOF (parent process closed the pipe)
- `SIGINT` / `SIGTERM`

On shutdown it gracefully stops the HTTP/HTTPS listeners (3 s timeout).
