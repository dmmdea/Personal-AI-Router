// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it, vi } from 'vitest'

vi.mock('electron', () => ({ BrowserWindow: { getAllWindows: () => [] } }))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))

import { getModularBridgeState } from '@/electron/service-bridge/modular-state'
import { subscribePush } from '@/electron/service-bridge/push-bus'
import { MEMORY_POOL_UNIFIED } from '@/shared/constants/hardware'
import type { NodeItemMetrics } from '@/shared/types/metrics'
import type { NodeMetricsHistory } from '@/ui/stores/metrics.store'
import { buildRadialChartMetrics } from '@/ui/utils/build-radial-chart-metrics'
import {
    isUnifiedMemory,
    memoryLabel,
    memoryLineValue,
    memoryUsedBytes,
    showsVram
} from '@/ui/utils/hardware-rows'

// Some devices in a node's inventory have no memory of their own — an Intel or
// AMD integrated GPU, an Arm Mali GPU, an RKNPU — and allocate out of a pool
// they share with the CPU. node-info marks those rows `memory_pool:"unified"`
// and sends a used figure ONLY when something measured one.
//
// It used to send the host's own RAM usage instead, and the card rendered it
// as the GPU's: an idle Intel UHD Graphics 630 read "VRAM 25.5 GB / 66 GB" on
// a 66 GB box, and an idle Mali-G610 read "VRAM 1.1 GB / 8 GB" on an 8 GB
// board. Both numbers were the whole machine's memory usage under the GPU's
// label.
//
// Dropping the fabricated number is only half a fix, though. The row must not
// then read "0 B / 66 GB" either, and the chart must not draw a flat zero line
// for it — both would assert an idle GPU just as confidently. So the rule has
// three parts, and every one of them is asserted below: mark the pool, omit
// what was never measured, and say "Shared <total>" rather than inventing a
// numerator.

/** The bridge's node-id -> gpu-row-index key, as toTopology / toMetrics build it. */
function gpuId(nodeUuid: string, index: number): string {
    return `${nodeUuid}:gpu:${index}`
}

/** Registers a node with the bridge so mergeNodeInfoResponse has somewhere to land. */
function announce(state: ReturnType<typeof getModularBridgeState>, uuid: string, ip: string) {
    state.handleNotification({
        source: 'broker',
        method: 'discovery:nodes-changed',
        params: { nodes: [{ hostUuid: uuid, name: uuid, ipAddress: ip, port: 14318 }] }
    })
}

describe('unified memory rows', () => {
    it('carries memory_pool through and emits no VRAM series for an unmeasured pool', () => {
        // The measured host: a discrete NVIDIA A2 plus the Coffee Lake iGPU
        // whose row carried the 25.5 GB. The iGPU arrives with a ceiling and
        // no vram_used_bytes, exactly as node-info now sends it.
        const state = getModularBridgeState()
        const frames: NodeItemMetrics[] = []
        const unsubscribe = subscribePush(event => {
            if (event.channel === 'metrics:update' && event.payload.id === 'uuid-igpu-1') {
                frames.push(event.payload)
            }
        })
        try {
            announce(state, 'uuid-igpu-1', '192.0.2.81')
            state.mergeNodeInfoResponse('uuid-igpu-1', {
                hostUuid: 'uuid-igpu-1',
                GPUs: [
                    {
                        name: 'NVIDIA A2',
                        vram_bytes: 16101933056,
                        vram_used_bytes: 490733568,
                        temperature_celsius: 64
                    },
                    {
                        name: 'Intel UHD Graphics 630 (Coffee Lake, Gen 9.5)',
                        vram_bytes: 70866960384,
                        memory_pool: 'unified'
                    }
                ],
                memory: { total_bytes: 70866960384, used_bytes: 26758635520 }
            })

            const topology = state.getNodesInitial().nodes['uuid-igpu-1'].topology
            expect(topology.gpus).toHaveLength(2)
            // The discrete card is unchanged: no marker at all.
            expect(topology.gpus[0].memoryPool).toBeUndefined()
            expect(topology.gpus[1]).toMatchObject({
                name: 'Intel UHD Graphics 630 (Coffee Lake, Gen 9.5)',
                vramTotal: 70866960384,
                memoryPool: MEMORY_POOL_UNIFIED
            })

            expect(frames.length).toBeGreaterThan(0)
            const latest = frames[frames.length - 1].current

            // The heart of it: one VRAM series, for the card that reported a
            // figure. The iGPU gets none — not an entry reading 0.
            expect(latest.gpuVramUsage).toHaveLength(1)
            expect(latest.gpuVramUsage[0].id).toBe(gpuId('uuid-igpu-1', 0))
            expect(latest.gpuVramUsage.map(entry => entry.id)).not.toContain(
                gpuId('uuid-igpu-1', 1)
            )

            // The node's own 26.7 GB of RAM in use is still reported as the
            // node's. It is only wearing the GPU's label that was ever wrong.
            const node = state.getAvailableNodes().find(entry => entry.id === 'uuid-igpu-1')
            expect(node).toBeDefined()
            expect(latest.memoryUsage).toBeGreaterThan(0)

            // Utilization series stay per-row and positional; dropping a VRAM
            // series must not have disturbed them.
            expect(latest.gpuUtilization.map(entry => entry.id)).toEqual([
                gpuId('uuid-igpu-1', 0),
                gpuId('uuid-igpu-1', 1)
            ])
        } finally {
            unsubscribe()
        }
    })

    it('keeps the VRAM series for a shared pool the device does measure', () => {
        // An AMD APU: its capacity is carve-out + GTT out of system RAM, so it
        // is marked unified — but amdgpu counts vram_used + gtt_used for that
        // device specifically, so the figure is real and must survive. The
        // same shape covers an Apple Silicon GPU and a DGX Spark part.
        const state = getModularBridgeState()
        const frames: NodeItemMetrics[] = []
        const unsubscribe = subscribePush(event => {
            if (event.channel === 'metrics:update' && event.payload.id === 'uuid-apu-1') {
                frames.push(event.payload)
            }
        })
        try {
            announce(state, 'uuid-apu-1', '192.0.2.82')
            state.mergeNodeInfoResponse('uuid-apu-1', {
                hostUuid: 'uuid-apu-1',
                GPUs: [
                    {
                        name: 'AMD Radeon Vega Graphics (Barcelo, GCN 5.1)',
                        vram_bytes: 17179869184,
                        vram_used_bytes: 3221225472,
                        utilization_percent: 12,
                        memory_pool: 'unified'
                    }
                ]
            })

            const topology = state.getNodesInitial().nodes['uuid-apu-1'].topology
            expect(topology.gpus[0].memoryPool).toBe(MEMORY_POOL_UNIFIED)

            const latest = frames[frames.length - 1].current
            expect(latest.gpuVramUsage).toHaveLength(1)
            expect(latest.gpuVramUsage[0].id).toBe(gpuId('uuid-apu-1', 0))
            // 3 GiB of 16 GiB.
            expect(latest.gpuVramUsage[0].value).toBeCloseTo(18.75, 5)
        } finally {
            unsubscribe()
        }
    })

    it('treats a row that gains or loses its pool marker as a change', () => {
        // The bridge skips the upsert when nothing looks different, so a node
        // whose node-info was upgraded mid-session would otherwise keep
        // rendering the old label forever.
        const state = getModularBridgeState()
        announce(state, 'uuid-igpu-2', '192.0.2.83')
        const before = {
            hostUuid: 'uuid-igpu-2',
            GPUs: [
                {
                    name: 'Intel UHD Graphics 630 (Coffee Lake, Gen 9.5)',
                    vram_bytes: 70866960384
                }
            ]
        }
        state.mergeNodeInfoResponse('uuid-igpu-2', before)
        expect(
            state.getNodesInitial().nodes['uuid-igpu-2'].topology.gpus[0].memoryPool
        ).toBeUndefined()

        state.mergeNodeInfoResponse('uuid-igpu-2', {
            ...before,
            GPUs: [{ ...before.GPUs[0], memory_pool: 'unified' }]
        })
        expect(state.getNodesInitial().nodes['uuid-igpu-2'].topology.gpus[0].memoryPool).toBe(
            MEMORY_POOL_UNIFIED
        )
    })

    it('labels a shared pool "Shared" and a dedicated one "VRAM"', () => {
        const igpu = { vramTotal: 70866960384, memoryPool: MEMORY_POOL_UNIFIED }
        const card = { vramTotal: 17179869184 }

        expect(isUnifiedMemory(igpu)).toBe(true)
        expect(isUnifiedMemory(card)).toBe(false)
        expect(memoryLabel(igpu)).toBe('Shared')
        expect(memoryLabel(card)).toBe('VRAM')

        // A shared pool still shows a memory line — it has a real ceiling.
        // Dropping the line would hide how much the device can reach.
        expect(showsVram(igpu)).toBe(true)
    })

    it('renders the ceiling alone when nothing measured the pool', () => {
        const igpu = { vramTotal: 70866960384, memoryPool: MEMORY_POOL_UNIFIED }
        // No series for this row -> no figure -> no numerator.
        expect(memoryUsedBytes(igpu, null)).toBeNull()
        expect(memoryLineValue(igpu, null)).toBe('66 GB')
        // Specifically NOT the two wrong renderings this replaced.
        expect(memoryLineValue(igpu, null)).not.toContain('0 B /')
        expect(memoryLineValue(igpu, null)).not.toContain('/')
    })

    it('renders used / total when the device did measure its pool', () => {
        const apu = { vramTotal: 17179869184, memoryPool: MEMORY_POOL_UNIFIED }
        const used = memoryUsedBytes(apu, 18.75)
        expect(used).toBe(3221225472)
        expect(memoryLineValue(apu, used)).toBe('3 GB / 16 GB')
        expect(memoryLabel(apu)).toBe('Shared')
    })

    it('leaves a discrete card reading 0 while it waits for its first sample', () => {
        // A missing series on a dedicated-memory row means "no sample yet",
        // not "unmeasurable", and that row has always shown 0 until one
        // arrives. Narrowing the fix to unified rows is the point.
        const card = { vramTotal: 17179869184 }
        expect(memoryUsedBytes(card, null)).toBe(0)
        expect(memoryLineValue(card, 0)).toBe('0 B / 16 GB')
        expect(memoryUsedBytes(card, 12.5)).toBe(2147483648)
    })

    it('plots no VRAM ring for a unified row with no figure, and keeps every other ring', () => {
        // The unified row is listed FIRST on purpose. With the old positional
        // lookup, gpuVramUsage[0] — the A2's 30% — would have been drawn as
        // the iGPU's ring, and the A2 would have got an empty one. Ordering it
        // this way is what makes that failure visible instead of accidental.
        const topology = {
            cpu: { model: 'Intel(R) Core(TM) i9-9900 CPU @ 3.10GHz', cores: 8, threads: 8 },
            gpus: [
                {
                    id: 'n:gpu:0',
                    name: 'Intel UHD Graphics 630 (Coffee Lake, Gen 9.5)',
                    vramTotal: 70866960384,
                    memoryPool: MEMORY_POOL_UNIFIED
                },
                { id: 'n:gpu:1', name: 'NVIDIA A2', vramTotal: 16101933056 }
            ],
            ram: 70866960384,
            storage: []
        }
        const sample = (value: number) => [{ timestamp: 1, value }]
        const metrics: NodeMetricsHistory = {
            gpuUtilization: [
                { id: 'n:gpu:0', data: sample(0) },
                { id: 'n:gpu:1', data: sample(41) }
            ],
            // Only the discrete card has one, exactly as the bridge emits it.
            gpuVramUsage: [{ id: 'n:gpu:1', data: sample(30) }],
            cpuUtilization: sample(4),
            memoryUsage: sample(38),
            gpuTemperature: [],
            cpuTemperature: 53
        }

        // iGPU: utilization only. A2: utilization + VRAM.
        const rings = buildRadialChartMetrics(topology, metrics)
        expect(rings).toHaveLength(3)
        expect(rings.map(ring => ring.value)).toEqual([0, 41, 30])

        // A discrete card with no sample yet keeps its ring at 0 — this is
        // narrowed to unified rows, not a blanket "hide empty rings".
        const cold: NodeMetricsHistory = { ...metrics, gpuVramUsage: [] }
        expect(buildRadialChartMetrics(topology, cold).map(ring => ring.value)).toEqual([0, 41, 0])
    })
})
