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
      "temperature_celsius": 61
    },
    {
      "name": "Google Coral Edge TPU",
      "utilization_percent": 30,
      "kind": "npu",
      "temperature_celsius": 52
    }
  ],
  "telemetryValid": true,
  "msSince": 137,
  "cpu": {
    "name": "AMD Ryzen 9 5900X 12-Core Processor",
    "cores": 12,
    "utilization_percent": 7,
    "temperature_celsius": 58
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
- `kind` is absent for a GPU and `"npu"` for a dedicated inference accelerator (an Edge TPU / NPU) that is listed in the same `GPUs` inventory so every client shows it. Accelerators cannot run the engines PAIR schedules, so consumers derive a node's GPU pressure with `noderec.MaxGPUUtilization`, which skips `kind:"npu"` rows, and an accelerator never contributes to `telemetryValid` / `msSince`.
- `temperature_celsius` is a device's thermal readout in whole degrees: on a GPU row from `nvidia-smi` (`temperature.gpu`, Linux and Windows; joined to the adapter by PCI address on Windows), on an accelerator row from its driver, and on `cpu` the package temperature from Linux hwmon (`coretemp` "Package id 0" / `k10temp` Tctl, else the `x86_pkg_temp` thermal zone). On Windows the package sensor is a ring-0 register, so the reading comes from the elevated `nvpair-sensors` service over `\\.\pipe\nvpair-sensors` (see `../nvpair-sensors/README.md`); a host without that service, without PawnIO, or with a stale report omits it. Every temperature field is omitted wherever it cannot be read.
- All dynamic fields and the `cpu` / `memory` objects use `omitempty`: a value the service couldn't read is dropped from the JSON entirely rather than reported as a misleading literal zero. A genuinely idle CPU renders the same as "unknown" — that ambiguity is intentional and benign.
- `vram_bytes` is reported through DXGI on Windows, `nvidia-smi` on Linux, and IORegistry on macOS. On a unified-memory NVIDIA GPU such as DGX Spark, Linux uses total physical system memory for `vram_bytes` and the independently sampled system-memory usage for `vram_used_bytes`. On Apple Silicon, `vram_bytes` is total physical unified memory and `vram_used_bytes` is the GPU driver's mapped allocation (`Alloc system memory`), not whole-system RAM usage or the momentarily active subset.

## Discovery

The service no longer advertises itself over mDNS. Its parent (the broker) registers the `ni` port with the `nvpair-node-scanner` discovery daemon, which carries it on this node's single `_nvpair-node` record; a peer's daemon then fetches `/v1/node-info` over plain HTTP to enrich the node. Transport is derived from the shared per-service policy (`nvpair-shared/noderec`): node-info is served plain on the broker path. The standalone BYO-TLS / `--cluster-dir` mTLS serving above is retained for running node-info on its own, but is unused under the broker.

## Platform Notes

- **Windows** (first-class): GPU inventory comes from DXGI (vendor-agnostic, includes VRAM). Dynamic CPU / VRAM-used / utilization / memory-used numbers come from a persistent PDH query plus `GlobalMemoryStatusEx`. GPU temperature comes from `nvidia-smi` on a 5 s poller, joined to each DXGI adapter through the display driver's PCI address (`D3DKMTQueryAdapterInfo` / `KMTQAITYPE_ADAPTERADDRESS`), so two identical cards never swap readings; a host without `nvidia-smi` reports none. The CPU package temperature is polled every 5 s from the `nvpair-sensors` service's named pipe (`nvpair-shared/hostsensors`): the sensor is a model-specific register that only the elevated helper can read through PawnIO. A report older than 30 s is dropped, an absent helper is retried every 30 s, and in both cases `cpu.temperature_celsius` is omitted.
- **Linux** (first-class): NVIDIA GPU inventory, dedicated VRAM usage, utilization and temperature come from `nvidia-smi`; CPU and system-memory usage come from `/proc`, the CPU package temperature from hwmon (`coretemp` / `k10temp` / `zenpower` / `cpu_thermal`, else the `x86_pkg_temp` thermal zone). Unified-memory GPUs use the `/proc/meminfo` system-memory snapshot even when dynamic `nvidia-smi` collection is unavailable. Non-NVIDIA adapters fall back to names from `ghw` without dynamic GPU stats. Inference accelerators behind the gasket/apex driver (Google Coral Edge TPU, `/sys/class/apex/*`) are listed with `kind:"npu"`; the driver keeps no busy counter, so `utilization_percent` is the fraction of 100 ms sub-intervals in the last second in which the device's `interrupt_counts` moved (the same "percent of time working" definition as `nvidia-smi`'s `utilization.gpu`, at coarser resolution), and `temperature_celsius` comes from its `temp` attribute.
- **macOS**: CPU and system-memory usage come from Mach through gopsutil's purego bindings. GPU identity, mapped memory, and utilization come from the built-in, unprivileged `/usr/sbin/ioreg` command's `IOAccelerator` `PerformanceStatistics`; no sudo or private framework binding is required. Apple Silicon is supported directly. Intel/AMD fields are best-effort when their drivers expose the same dedicated-memory counters. The performance keys are undocumented and may change across macOS releases; a missing or changed key leaves only that metric out and does not stop CPU or memory collection.
- **Other platforms**: GPU names come from `ghw`; VRAM and dynamic stats are not reported.

## Shutdown

The service shuts down on:

- stdin EOF (parent process closed the pipe)
- `SIGINT` / `SIGTERM`

On shutdown it gracefully stops the HTTP/HTTPS listeners (3 s timeout).
