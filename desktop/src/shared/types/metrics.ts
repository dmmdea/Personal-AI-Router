// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

export interface GpuMetricValue {
    id: string // GPU ID from topology
    value: number // percentage
}

export interface NodeItemMetricsEntry {
    timestamp: number // milliseconds since epoch
    cpuUtilization: number // percentage
    memoryUsage: number // percentage
    gpuUtilization: GpuMetricValue[] // percentage per GPU
    gpuVramUsage: GpuMetricValue[] // percentage per GPU
    // Whole degrees Celsius. 0 = the node reports no reading for that device
    // (node-info omits temperature_celsius where it has no driverless source);
    // the UI shows "--" rather than a literal zero. Absent on frames from a
    // backend that predates the field.
    gpuTemperature?: GpuMetricValue[] // degrees per GPU
    cpuTemperature?: number // degrees
    // Whole watts. 0 = nothing meters that device, which is most of a node's
    // inventory: node-info omits power_watts for an integrated GPU, a Mali
    // GPU, an RKNPU and an Edge TPU, and for any CPU whose
    // energy counter it cannot read. The UI shows no figure for a 0 rather
    // than "0 W", which would read as a device drawing nothing. Absent on
    // frames from a backend that predates the field.
    gpuPower?: GpuMetricValue[] // watts per GPU
    cpuPower?: number // watts
}

export interface NodeItemMetrics {
    id: string
    current: NodeItemMetricsEntry
    historical: NodeItemMetricsEntry[]
}
