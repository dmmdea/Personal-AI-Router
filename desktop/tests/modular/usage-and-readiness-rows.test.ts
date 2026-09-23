// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it, vi } from 'vitest'

vi.mock('electron', () => ({ BrowserWindow: { getAllWindows: () => [] } }))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))

import { getModularBridgeState } from '@/electron/service-bridge/modular-state'
import { subscribePush } from '@/electron/service-bridge/push-bus'
import { MEMORY_POOL_UNIFIED } from '@/shared/constants/hardware'
import type { NodeItemMetrics } from '@/shared/types/metrics'
import type { NodeItem } from '@/shared/types/nodes'
import type { NodeMetricsHistory } from '@/ui/stores/metrics.store'
import { buildRadialChartMetrics } from '@/ui/utils/build-radial-chart-metrics'
import { hasInferenceReadyGpu } from '@/ui/utils/gpu-inference'
import { hasFirstMetrics } from '@/ui/utils/has-first-metrics'
import { formatUsage, usagePercent } from '@/ui/utils/hardware-rows'

// Two per-row facts node-info now states outright, because the absence of a
// field could not carry them:
//
//  - utilization_unavailable: the device has no busy counter (a Hailo module,
//    a Linux Intel GPU, an unreadable Rockchip counter). utilization_percent is
//    omitempty, so a measured idle 0 is absent on the wire too; reading every
//    absence as "unmeasured" would blank the Usage line of every idle GPU on
//    the fleet, and reading every absence as 0 printed "Usage 0%" for devices
//    nobody measured. The flag separates them: flagged rows show "—" and get
//    no series or ring; unflagged rows (and every row from an older node) keep
//    reading an absence as 0 %.
//  - inference_ready: false on a device no engine can use (a Mali GPU, an NPU,
//    an Edge TPU, a Linux Intel iGPU). A board whose only devices are those
//    must fall back to the CPU + RAM rings instead of charting them as GPUs.

function gpuId(nodeUuid: string, index: number): string {
    return `${nodeUuid}:gpu:${index}`
}

function announce(state: ReturnType<typeof getModularBridgeState>, uuid: string, ip: string) {
    state.handleNotification({
        source: 'broker',
        method: 'discovery:nodes-changed',
        params: { nodes: [{ hostUuid: uuid, name: uuid, ipAddress: ip, port: 14318 }] }
    })
}

function latestFrame(uuid: string, merge: () => void): NodeItemMetrics['current'] {
    const frames: NodeItemMetrics[] = []
    const unsubscribe = subscribePush(event => {
        if (event.channel === 'metrics:update' && event.payload.id === uuid) {
            frames.push(event.payload)
        }
    })
    try {
        merge()
    } finally {
        unsubscribe()
    }
    expect(frames.length).toBeGreaterThan(0)
    return frames[frames.length - 1].current
}

const sample = (value: number) => [{ timestamp: 1, value }]

describe('rows with no utilization source', () => {
    it('emits no usage series for a flagged row and keeps an idle GPU at 0 %', () => {
        // A desktop with an idle RTX 5060 (no utilization_percent: it measured
        // 0), a UHD 630 and a Hailo-8L that has no busy counter at all.
        const state = getModularBridgeState()
        announce(state, 'uuid-hailo-1', '192.0.2.91')
        const current = latestFrame('uuid-hailo-1', () =>
            state.mergeNodeInfoResponse('uuid-hailo-1', {
                hostUuid: 'uuid-hailo-1',
                GPUs: [
                    { name: 'NVIDIA GeForce RTX 5060', vram_bytes: 8279556096 },
                    {
                        name: 'Intel UHD Graphics 630 (Coffee Lake, Gen 9.5)',
                        vram_bytes: 51539607552,
                        memory_pool: 'unified',
                        utilization_percent: 3
                    },
                    {
                        name: 'Hailo-8L AI Accelerator',
                        kind: 'npu',
                        temperature_celsius: 47,
                        utilization_unavailable: true,
                        inference_ready: false
                    }
                ]
            })
        )

        // Idle NVIDIA and the measured iGPU keep a series; the Hailo has none.
        expect(current.gpuUtilization).toEqual([
            { id: gpuId('uuid-hailo-1', 0), value: 0 },
            { id: gpuId('uuid-hailo-1', 1), value: 3 }
        ])
        // Its temperature still travels.
        expect(current.gpuTemperature?.find(t => t.id === gpuId('uuid-hailo-1', 2))?.value).toBe(47)

        const topology = state.getNodesInitial().nodes['uuid-hailo-1'].topology
        expect(topology.gpus[2].utilizationUnavailable).toBe(true)
        expect(topology.gpus[0].utilizationUnavailable).toBeUndefined()
        expect(topology.gpus[1].utilizationUnavailable).toBeUndefined()
    })

    it('keeps reading an absent utilization as 0 % from a node that predates the flag', () => {
        // A node on an older node-info sends an idle Hailo exactly as it sends
        // an idle GPU. Nothing distinguishes them, so both keep their series —
        // the pre-flag behaviour, unchanged.
        const state = getModularBridgeState()
        announce(state, 'uuid-old-1', '192.0.2.92')
        const current = latestFrame('uuid-old-1', () =>
            state.mergeNodeInfoResponse('uuid-old-1', {
                hostUuid: 'uuid-old-1',
                GPUs: [
                    { name: 'NVIDIA GeForce RTX 5060', vram_bytes: 8279556096 },
                    { name: 'Hailo-8L AI Accelerator', kind: 'npu', temperature_celsius: 47 }
                ]
            })
        )
        expect(current.gpuUtilization.map(entry => entry.id)).toEqual([
            gpuId('uuid-old-1', 0),
            gpuId('uuid-old-1', 1)
        ])
        const topology = state.getNodesInitial().nodes['uuid-old-1'].topology
        expect(topology.inferenceHardwareIds).toBeUndefined()
    })

    it('treats a row gaining the flag as a change', () => {
        const state = getModularBridgeState()
        announce(state, 'uuid-flag-1', '192.0.2.93')
        const before = {
            hostUuid: 'uuid-flag-1',
            GPUs: [{ name: 'Intel UHD Graphics 630 (Coffee Lake, Gen 9.5)', vram_bytes: 1 }]
        }
        state.mergeNodeInfoResponse('uuid-flag-1', before)
        state.mergeNodeInfoResponse('uuid-flag-1', {
            ...before,
            GPUs: [{ ...before.GPUs[0], utilization_unavailable: true }]
        })
        expect(
            state.getNodesInitial().nodes['uuid-flag-1'].topology.gpus[0].utilizationUnavailable
        ).toBe(true)
    })

    it('renders "—" for a flagged row and a number for every other', () => {
        const hailo = { kind: 'npu', vramTotal: 0, utilizationUnavailable: true }
        const idleGpu = { vramTotal: 8279556096 }
        expect(usagePercent(hailo, null)).toBeNull()
        // Even a stray value never turns a flagged row back into a reading.
        expect(usagePercent(hailo, 0)).toBeNull()
        expect(formatUsage(usagePercent(hailo, null))).toBe('—')

        // An unflagged row with no sample yet reads 0, as it always has.
        expect(usagePercent(idleGpu, null)).toBe(0)
        expect(formatUsage(usagePercent(idleGpu, null))).toBe('0%')
        expect(formatUsage(usagePercent(idleGpu, 71.6))).toBe('71%')
    })

    it('draws no usage ring for a flagged row and keeps the others on their own series', () => {
        // The flagged row is FIRST, so a positional lookup would hand its
        // (absent) series slot to the discrete card behind it.
        const topology = {
            cpu: { model: 'CPU', cores: 8, threads: 8 },
            gpus: [
                {
                    id: 'n:gpu:0',
                    name: 'Intel Arc B580 (Battlemage, Xe2-HPG)',
                    vramTotal: 12884901888,
                    utilizationUnavailable: true
                },
                { id: 'n:gpu:1', name: 'NVIDIA A2', vramTotal: 16101933056 }
            ],
            ram: 67282386944,
            storage: []
        }
        const metrics: NodeMetricsHistory = {
            gpuUtilization: [{ id: 'n:gpu:1', data: sample(41) }],
            gpuVramUsage: [
                { id: 'n:gpu:0', data: sample(10) },
                { id: 'n:gpu:1', data: sample(30) }
            ],
            cpuUtilization: sample(4),
            memoryUsage: sample(36),
            gpuTemperature: [],
            cpuTemperature: 53,
            gpuPower: [],
            cpuPower: 0
        }
        // Arc: VRAM ring only. A2: usage 41 + VRAM 30.
        expect(buildRadialChartMetrics(topology, metrics).map(ring => ring.value)).toEqual([
            10, 41, 30
        ])
    })

    it('does not wait forever for a usage series that will never come', () => {
        // The node's only inference-ready GPU has no busy counter, so the
        // bridge sends no utilization series; the chart must still render.
        const node: NodeItem = {
            id: 'n',
            name: 'n',
            status: 'active',
            ipAddress: '192.0.2.94',
            port: 14318,
            allIpAddresses: ['192.0.2.94'],
            os: 'Linux',
            topology: {
                cpu: { model: 'CPU', cores: 8, threads: 8 },
                gpus: [
                    {
                        id: 'n:gpu:0',
                        name: 'Intel Arc B580 (Battlemage, Xe2-HPG)',
                        vramTotal: 12884901888,
                        utilizationUnavailable: true
                    }
                ],
                ram: 1,
                storage: []
            }
        }
        const metrics: NodeMetricsHistory = {
            gpuUtilization: [],
            gpuVramUsage: [],
            cpuUtilization: sample(4),
            memoryUsage: sample(36),
            gpuTemperature: [],
            cpuTemperature: 0,
            gpuPower: [],
            cpuPower: 0
        }
        expect(hasFirstMetrics(node, metrics)).toBe(true)
        // An ordinary GPU still waits for its first sample.
        const ordinary: NodeItem = {
            ...node,
            topology: {
                ...node.topology,
                gpus: [{ id: 'n:gpu:0', name: 'NVIDIA A2', vramTotal: 16101933056 }]
            }
        }
        expect(hasFirstMetrics(ordinary, metrics)).toBe(false)
    })
})

describe('rows no engine can use', () => {
    it('falls back to CPU and RAM on a board whose devices are a Mali GPU and an NPU', () => {
        const state = getModularBridgeState()
        announce(state, 'uuid-opi-1', '192.0.2.95')
        state.mergeNodeInfoResponse('uuid-opi-1', {
            hostUuid: 'uuid-opi-1',
            GPUs: [
                {
                    name: 'Arm Mali-G610 MP4',
                    vram_bytes: 16466812928,
                    memory_pool: 'unified',
                    inference_ready: false
                },
                {
                    name: 'Rockchip RK3588S NPU (3 cores)',
                    kind: 'npu',
                    vram_bytes: 16466812928,
                    memory_pool: 'unified',
                    inference_ready: false
                }
            ],
            cpu: { name: 'Rockchip RK3588S (Orange Pi 5)', cores: 8, utilization_percent: 12 },
            memory: { total_bytes: 16466812928, used_bytes: 2147483648 }
        })

        const topology = state.getNodesInitial().nodes['uuid-opi-1'].topology
        expect(topology.inferenceHardwareIds).toEqual([])
        expect(hasInferenceReadyGpu(topology)).toBe(false)

        const metrics: NodeMetricsHistory = {
            gpuUtilization: [
                { id: 'uuid-opi-1:gpu:0', data: sample(0) },
                { id: 'uuid-opi-1:gpu:1', data: sample(0) }
            ],
            gpuVramUsage: [],
            cpuUtilization: sample(12),
            memoryUsage: sample(13),
            gpuTemperature: [],
            cpuTemperature: 40,
            gpuPower: [],
            cpuPower: 0
        }
        // CPU + RAM rings, not two idle GPU rings.
        expect(buildRadialChartMetrics(topology, metrics).map(ring => ring.value)).toEqual([12, 13])
    })

    it('keeps the ready rows of a mixed node, by their post-sort ids', () => {
        // node-info lists the Coral and the Intel iGPU before the NVIDIA card
        // in no particular order; the bridge sorts NVIDIA first, and the
        // readiness list must follow the rows to their new positions.
        const state = getModularBridgeState()
        announce(state, 'uuid-mixed-1', '192.0.2.96')
        state.mergeNodeInfoResponse('uuid-mixed-1', {
            hostUuid: 'uuid-mixed-1',
            GPUs: [
                {
                    name: 'Intel UHD Graphics 630 (Coffee Lake, Gen 9.5)',
                    vram_bytes: 67282386944,
                    memory_pool: 'unified',
                    utilization_unavailable: true,
                    inference_ready: false
                },
                { name: 'NVIDIA A2', vram_bytes: 16101933056 },
                { name: 'Google Coral Edge TPU', kind: 'npu', inference_ready: false }
            ]
        })
        const topology = state.getNodesInitial().nodes['uuid-mixed-1'].topology
        expect(topology.gpus.map(gpu => gpu.name)).toEqual([
            'NVIDIA A2',
            'Intel UHD Graphics 630 (Coffee Lake, Gen 9.5)',
            'Google Coral Edge TPU'
        ])
        expect(topology.inferenceHardwareIds).toEqual([gpuId('uuid-mixed-1', 0)])
        expect(topology.gpus[1].memoryPool).toBe(MEMORY_POOL_UNIFIED)
    })

    it('lets a node-level readiness list win over the row flags', () => {
        const state = getModularBridgeState()
        announce(state, 'uuid-list-1', '192.0.2.97')
        state.mergeNodeInfoResponse('uuid-list-1', {
            hostUuid: 'uuid-list-1',
            inference_hardware_ids: ['uuid-list-1:gpu:1'],
            GPUs: [
                { name: 'NVIDIA A2', vram_bytes: 16101933056 },
                { name: 'Arm Mali-G610 MP4', inference_ready: false }
            ]
        })
        expect(state.getNodesInitial().nodes['uuid-list-1'].topology.inferenceHardwareIds).toEqual([
            'uuid-list-1:gpu:1'
        ])
    })
})
