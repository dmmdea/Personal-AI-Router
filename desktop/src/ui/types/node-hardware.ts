// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

export type GpuInfo = {
    id: string
    name: string
    // hliId: string;
    vramTotal: number
    // Bytes this device is holding, or null when the node reports no figure
    // for it — a shared-pool device whose driver counts nothing. The memory
    // line then shows the ceiling alone; see ui/utils/hardware-rows.ts.
    vramUsedBytes: number | null
    usage: number // percentage
    usageColor: string
    vramColor: string
    temperature: number // degrees Celsius, 0 = no reading
    // Whole watts the device is drawing, 0 = nothing meters it. Shown beside
    // the temperature ("50 °C (200 W)"), or on a line of its own when the
    // device is metered but has no thermal readout.
    power: number
    // "npu" for an accelerator row (no VRAM) or "board" for the motherboard
    // controller (no VRAM and no usage); absent for a GPU.
    kind?: string
    // "unified" when vramTotal is a pool shared with the host; absent for a
    // discrete card. Drives the "Shared" vs "VRAM" label.
    memoryPool?: string
}

export type CpuFallbackInfo = {
    model: string
    usage: number // percentage 0-100
    usageColor: string
    temperature: number // degrees Celsius, 0 = no reading
    power: number // whole watts, 0 = no reading
    memoryUsage: number // percentage 0-100
    memoryUsageFormatted: string
    memoryTotalFormatted: string
    memoryColor: string
}
