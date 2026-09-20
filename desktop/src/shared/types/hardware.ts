// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

export interface GpuInfo {
    id: string
    name: string
    vramTotal: number // in bytes
    // Absent for a GPU. "npu" for a dedicated inference accelerator (Edge TPU
    // / NPU) and "board" for the host's motherboard controller, both of which
    // node-info lists beside the GPUs. Neither has a VRAM figure and the board
    // has no busy counter either, so the UI drops those lines for them — see
    // ui/utils/hardware-rows.ts.
    kind?: string
    // MEMORY_POOL_UNIFIED when vramTotal is a pool the device shares with the
    // host (an integrated GPU, an Apple Silicon GPU, a UMA part) rather than
    // memory of its own; absent for a discrete card. The UI labels such a row
    // "Shared" instead of "VRAM", because the same bytes are the CPU's - and
    // pairs it with a used figure only when the node reported one.
    memoryPool?: string
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
