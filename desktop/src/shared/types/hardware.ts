// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

export interface GpuInfo {
    id: string
    name: string
    vramTotal: number // in bytes
    // Absent for a GPU. "npu" for a dedicated inference accelerator (Edge TPU
    // / NPU), which node-info lists beside the GPUs. It may have no VRAM
    // figure, and the UI then drops that line — see ui/utils/hardware-rows.ts.
    kind?: string
    // MEMORY_POOL_UNIFIED when vramTotal is a pool the device shares with the
    // host (an integrated GPU, an Apple Silicon GPU, a UMA part) rather than
    // memory of its own; absent for a discrete card. The UI labels such a row
    // "Shared" instead of "VRAM", because the same bytes are the CPU's - and
    // pairs it with a used figure only when the node reported one.
    memoryPool?: string
    // True when the node says this device has no busy counter it can read (a
    // Hailo module, a Linux Intel GPU, an unreadable Rockchip counter). Such a
    // row has no utilization series, and the UI shows its usage as unknown
    // ("—") rather than as an idle 0 %; absent on every row with a source.
    utilizationUnavailable?: boolean
}

export interface StorageInfo {
    name: string
    capacity: number // in bytes
}

export interface CPUInfo {
    model: string
    cores: number
    threads: number
}

export interface SystemTopology {
    cpu: CPUInfo
    gpus: GpuInfo[]
    ram: number
    storage: StorageInfo[]
    // Backend-provided list of inference-ready hardware ids (node-level). The
    // node charts show exactly the GPUs whose id is in this list and do NO
    // client-side classification. When absent (the backend does not report
    // readiness yet) the UI shows all GPUs. See the routing limitations in
    // docs/services-parity.md.
    inferenceHardwareIds?: string[]
}
