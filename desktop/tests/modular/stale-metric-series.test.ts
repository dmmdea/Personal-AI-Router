// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, describe, expect, it } from 'vitest'

import type { GpuMetricValue, NodeItemMetrics } from '@/shared/types/metrics'
import { useMetricsStore } from '@/ui/stores/metrics.store'

// The metrics store appended per-GPU history keyed by id and never removed an
// entry, so a row that stopped reporting kept its last value on the card until
// the app was restarted. Measured live: a shared-pool GPU whose node stopped
// sending a used figure went on reading "Shared 25.1 GB / 66 GB" — a number
// nothing was measuring any more — because the entry with that id was still
// in gpuVramUsage.
//
// The fix drops entries whose id is absent from the latest snapshot. The rule
// is presence, not value, which is what keeps a dedicated GPU's line from
// blinking out on a sample it happened to report as zero.

type MetricsListener = (metrics: NodeItemMetrics) => void

interface FakeWindow {
    pairApi: {
        nodes: { onRemove: (cb: (nodeId: string) => void) => () => void }
        metrics: { onUpdate: (cb: MetricsListener) => () => void }
    }
}

/**
 * Installs the preload bridge the store subscribes to and returns an emitter
 * for the metrics channel. The store only ever reads window.pairApi inside
 * initialize(), so a plain object is enough.
 */
function installPairApi(): (metrics: NodeItemMetrics) => void {
    let listener: MetricsListener = () => {}
    const fake: FakeWindow = {
        pairApi: {
            nodes: { onRemove: () => () => {} },
            metrics: {
                onUpdate: cb => {
                    listener = cb
                    return () => {}
                }
            }
        }
    }
    ;(globalThis as unknown as { window: FakeWindow }).window = fake
    useMetricsStore.getState().clearAll()
    useMetricsStore.getState().initialize()
    return metrics => listener(metrics)
}

function frame(
    timestamp: number,
    gpuUtilization: GpuMetricValue[],
    gpuVramUsage: GpuMetricValue[],
    extras: Partial<NodeItemMetrics['current']> = {}
): NodeItemMetrics {
    const current = {
        timestamp,
        cpuUtilization: 7,
        memoryUsage: 38,
        gpuUtilization,
        gpuVramUsage,
        ...extras
    }
    return { id: 'node-1', current, historical: [current] }
}

function history() {
    const entry = useMetricsStore.getState().nodeMetrics.get('node-1')
    if (!entry) throw new Error('no history for node-1')
    return entry
}

afterEach(() => {
    useMetricsStore.getState().cleanup()
    useMetricsStore.getState().clearAll()
    delete (globalThis as unknown as { window?: FakeWindow }).window
})

describe('stale metric series', () => {
    it('drops a VRAM series whose row stopped reporting', () => {
        const emit = installPairApi()

        emit(
            frame(
                1000,
                [
                    { id: 'node-1:gpu:0', value: 41 },
                    { id: 'node-1:gpu:1', value: 3 }
                ],
                [
                    { id: 'node-1:gpu:0', value: 12 },
                    // The shared-pool row, while its node still measured it.
                    { id: 'node-1:gpu:1', value: 38 }
                ]
            )
        )
        expect(history().gpuVramUsage.map(entry => entry.id)).toEqual([
            'node-1:gpu:0',
            'node-1:gpu:1'
        ])

        // The node stops measuring that pool: the bridge emits no series for
        // it at all (reportsMemoryUsage in modular-state.ts).
        emit(
            frame(
                2000,
                [
                    { id: 'node-1:gpu:0', value: 44 },
                    { id: 'node-1:gpu:1', value: 2 }
                ],
                [{ id: 'node-1:gpu:0', value: 13 }]
            )
        )

        const after = history()
        expect(after.gpuVramUsage.map(entry => entry.id)).toEqual(['node-1:gpu:0'])
        // Its utilization is still reported, so that series stays: the two
        // are pruned against their own snapshots, not against each other.
        expect(after.gpuUtilization.map(entry => entry.id)).toEqual([
            'node-1:gpu:0',
            'node-1:gpu:1'
        ])
    })

    it('drops every series for a GPU row that disappears', () => {
        const emit = installPairApi()
        emit(
            frame(
                1000,
                [
                    { id: 'node-1:gpu:0', value: 41 },
                    { id: 'node-1:gpu:1', value: 9 }
                ],
                [
                    { id: 'node-1:gpu:0', value: 12 },
                    { id: 'node-1:gpu:1', value: 20 }
                ]
            )
        )
        emit(frame(2000, [{ id: 'node-1:gpu:0', value: 44 }], [{ id: 'node-1:gpu:0', value: 13 }]))

        const after = history()
        expect(after.gpuUtilization.map(entry => entry.id)).toEqual(['node-1:gpu:0'])
        expect(after.gpuVramUsage.map(entry => entry.id)).toEqual(['node-1:gpu:0'])
        // The surviving row keeps its accumulated history rather than being
        // rebuilt: pruning must not cost the chart its past.
        expect(after.gpuUtilization[0].data.length).toBeGreaterThan(1)
        expect(after.gpuUtilization[0].data[after.gpuUtilization[0].data.length - 1].value).toBe(44)
    })

    it('keeps a dedicated GPU line through a zero sample', () => {
        // Presence, not value. A card that reports 0% and 0 bytes for one
        // tick is idle, not gone, and its line must not blink out and back.
        const emit = installPairApi()
        emit(frame(1000, [{ id: 'node-1:gpu:0', value: 41 }], [{ id: 'node-1:gpu:0', value: 12 }]))
        emit(frame(2000, [{ id: 'node-1:gpu:0', value: 0 }], [{ id: 'node-1:gpu:0', value: 0 }]))

        const after = history()
        expect(after.gpuUtilization.map(entry => entry.id)).toEqual(['node-1:gpu:0'])
        expect(after.gpuVramUsage.map(entry => entry.id)).toEqual(['node-1:gpu:0'])
    })

    it('replaces the temperature and power readings wholesale', () => {
        // These two carry only the latest snapshot, so a row that disappears
        // is already gone from them — the property the prune relies on.
        const emit = installPairApi()
        emit(
            frame(
                1000,
                [
                    { id: 'node-1:gpu:0', value: 41 },
                    { id: 'node-1:gpu:1', value: 9 }
                ],
                [{ id: 'node-1:gpu:0', value: 12 }],
                {
                    gpuTemperature: [
                        { id: 'node-1:gpu:0', value: 61 },
                        { id: 'node-1:gpu:1', value: 52 }
                    ],
                    gpuPower: [
                        { id: 'node-1:gpu:0', value: 210 },
                        { id: 'node-1:gpu:1', value: 21 }
                    ],
                    cpuTemperature: 55,
                    cpuPower: 140
                }
            )
        )
        expect(history().gpuPower.map(entry => entry.id)).toEqual(['node-1:gpu:0', 'node-1:gpu:1'])
        expect(history().cpuPower).toBe(140)

        emit(
            frame(2000, [{ id: 'node-1:gpu:0', value: 44 }], [{ id: 'node-1:gpu:0', value: 13 }], {
                gpuTemperature: [{ id: 'node-1:gpu:0', value: 63 }],
                gpuPower: [{ id: 'node-1:gpu:0', value: 215 }],
                cpuTemperature: 56,
                cpuPower: 0
            })
        )
        const after = history()
        expect(after.gpuTemperature).toEqual([{ id: 'node-1:gpu:0', value: 63 }])
        expect(after.gpuPower).toEqual([{ id: 'node-1:gpu:0', value: 215 }])
        // A host that stops reporting CPU power reports none, not the last
        // figure it managed to read.
        expect(after.cpuPower).toBe(0)
    })

    it('ignores a repeated frame rather than pruning on it', () => {
        // The store short-circuits a frame carrying the timestamp it already
        // holds. Nothing may change on that path, prune included.
        const emit = installPairApi()
        emit(
            frame(
                1000,
                [
                    { id: 'node-1:gpu:0', value: 41 },
                    { id: 'node-1:gpu:1', value: 9 }
                ],
                [
                    { id: 'node-1:gpu:0', value: 12 },
                    { id: 'node-1:gpu:1', value: 20 }
                ]
            )
        )
        emit(frame(1000, [], []))
        expect(history().gpuUtilization.map(entry => entry.id)).toEqual([
            'node-1:gpu:0',
            'node-1:gpu:1'
        ])
    })
})
