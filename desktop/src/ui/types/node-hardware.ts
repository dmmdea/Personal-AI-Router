// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

export type GpuInfo = {
    id: string
    name: string
    // hliId: string;
    vramTotal: number
    vramTotalFormatted: string
    vramUsageFormatted: string
    vramUsage: number // percentage
    usage: number // percentage
    usageColor: string
    vramColor: string
    temperature: number // degrees Celsius, 0 = no reading
    kind?: string // "npu" for an accelerator row (no VRAM)
}

export type CpuFallbackInfo = {
    model: string
    usage: number // percentage 0-100
    usageColor: string
    temperature: number // degrees Celsius, 0 = no reading
    memoryUsage: number // percentage 0-100
    memoryUsageFormatted: string
    memoryTotalFormatted: string
    memoryColor: string
}
