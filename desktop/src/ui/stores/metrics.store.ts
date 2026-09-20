// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { create } from 'zustand'
import type { GpuMetricValue, NodeItemMetrics } from '@/shared/types/metrics'
import type { PerformanceMetric } from '@/ui/types/types'

const MAX_DATA_POINTS = 60

interface GpuMetricsHistory {
    id: string
    data: PerformanceMetric[]
}

export interface NodeMetricsHistory {
    gpuUtilization: GpuMetricsHistory[]
    gpuVramUsage: GpuMetricsHistory[]
    cpuUtilization: PerformanceMetric[]
    memoryUsage: PerformanceMetric[]
    // Latest readings only (degrees Celsius; 0 = none reported) — shown as
    // numbers next to the legend, not charted.
    gpuTemperature: GpuMetricValue[]
    cpuTemperature: number
    // Same shape for power draw (whole watts; 0 = nothing meters that
    // device). Replaced wholesale from each snapshot rather than appended,
    // which is also why neither of these can go stale the way the charted
    // series could — see pruneSeries.
    gpuPower: GpuMetricValue[]
    cpuPower: number
}

interface MetricsStore {
    nodeMetrics: Map<string, NodeMetricsHistory>
    generation: number
    initialize: () => void
    clearAll: () => void
    cleanup: () => void
}

let unsubs: Array<() => void> = []

function pushMetric(arr: PerformanceMetric[], value: number | undefined, timestamp: number): void {
    if (arr.length >= MAX_DATA_POINTS) {
        arr.shift()
    }
    arr.push({ timestamp, value: value ?? 0 })
}

function createPrefill(baseTs: number): PerformanceMetric[] {
    return Array.from({ length: MAX_DATA_POINTS }, (_, i) => ({
        timestamp: baseTs - (MAX_DATA_POINTS - i) * 1000,
        value: 0
    }))
}

/**
 * Drops history for series the node is no longer reporting.
 *
 * Entries were only ever added here, never removed, so a row that stopped
 * reporting kept its last value on screen until the app restarted. That is
 * exactly what happened when node-info stopped sending a used figure for a
 * shared-pool GPU: the card went on reading "Shared 25.1 GB / 66 GB" —
 * a measurement nobody was taking any more — because the entry with that id
 * was still in this array.
 *
 * The rule is presence in the current snapshot, not the value in it. A
 * dedicated GPU that misses one sample still appears in `present` (the
 * bridge emits an entry for every row it knows), so its line does not blink
 * out and back; only a series whose id is genuinely gone is dropped.
 */
function pruneSeries(series: GpuMetricsHistory[], present: GpuMetricValue[]): GpuMetricsHistory[] {
    const ids = new Set(present.map(entry => entry.id))
    return series.filter(entry => ids.has(entry.id))
}

export const useMetricsStore = create<MetricsStore>((set, get) => ({
    nodeMetrics: new Map(),
    generation: 0,

    clearAll: () => {
        set({ nodeMetrics: new Map(), generation: get().generation + 1 })
    },

    initialize: () => {
        if (!window.pairApi) return

        unsubs.push(
            window.pairApi.nodes.onRemove((nodeId: string) => {
                const map = get().nodeMetrics
                if (map.has(nodeId)) {
                    map.delete(nodeId)
                    set({ generation: get().generation + 1 })
                }
            }),
            window.pairApi.metrics.onUpdate((metrics: NodeItemMetrics) => {
                const map = get().nodeMetrics
                let history = map.get(metrics.id)

                if (!history) {
                    const ts = metrics.current.timestamp
                    history = {
                        gpuUtilization: metrics.current.gpuUtilization.map(gpu => ({
                            id: gpu.id,
                            data: createPrefill(ts)
                        })),
                        gpuVramUsage: metrics.current.gpuVramUsage.map(gpu => ({
                            id: gpu.id,
                            data: createPrefill(ts)
                        })),
                        cpuUtilization: createPrefill(ts),
                        memoryUsage: createPrefill(ts),
                        gpuTemperature: [],
                        cpuTemperature: 0,
                        gpuPower: [],
                        cpuPower: 0
                    }
                    map.set(metrics.id, history)
                }

                const lastTs = history.cpuUtilization[history.cpuUtilization.length - 1]?.timestamp
                if (lastTs === metrics.current.timestamp) return

                const ts = metrics.current.timestamp

                for (const gpu of metrics.current.gpuUtilization) {
                    let entry = history.gpuUtilization.find(h => h.id === gpu.id)
                    if (!entry) {
                        entry = { id: gpu.id, data: [] }
                        history.gpuUtilization.push(entry)
                    }
                    pushMetric(entry.data, gpu.value, ts)
                }

                for (const gpu of metrics.current.gpuVramUsage) {
                    let entry = history.gpuVramUsage.find(h => h.id === gpu.id)
                    if (!entry) {
                        entry = { id: gpu.id, data: [] }
                        history.gpuVramUsage.push(entry)
                    }
                    pushMetric(entry.data, gpu.value, ts)
                }

                pushMetric(history.cpuUtilization, metrics.current.cpuUtilization, ts)
                pushMetric(history.memoryUsage, metrics.current.memoryUsage, ts)

                // Drop series the node has stopped reporting. Without this a
                // row that goes quiet keeps its last value on the card for
                // the life of the process.
                history.gpuUtilization = pruneSeries(
                    history.gpuUtilization,
                    metrics.current.gpuUtilization
                )
                history.gpuVramUsage = pruneSeries(
                    history.gpuVramUsage,
                    metrics.current.gpuVramUsage
                )

                // Temperature and power are the whole snapshot each time, so
                // an id that disappears is already gone from them.
                history.gpuTemperature = metrics.current.gpuTemperature ?? []
                history.cpuTemperature = metrics.current.cpuTemperature ?? 0
                history.gpuPower = metrics.current.gpuPower ?? []
                history.cpuPower = metrics.current.cpuPower ?? 0

                map.set(metrics.id, { ...history })
                set({ generation: get().generation + 1 })
            })
        )
    },

    cleanup: () => {
        unsubs.forEach(u => u())
        unsubs = []
    }
}))
