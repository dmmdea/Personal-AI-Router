// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it, vi } from 'vitest'

vi.mock('electron', () => ({ BrowserWindow: { getAllWindows: () => [] } }))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))

import { getModularBridgeState } from '@/electron/service-bridge/modular-state'
import { subscribePush } from '@/electron/service-bridge/push-bus'
import type { NodeItemMetrics } from '@/shared/types/metrics'

// node-info reports temperature_celsius on GPU rows, on accelerator rows
// (kind:"npu") and on cpu. The bridge must carry all three into the metrics
// frame, tag the accelerator in the topology, and treat a temperature-only
// change as new telemetry (otherwise a warming GPU never repaints).
describe('temperature passthrough', () => {
    it('carries GPU, accelerator and CPU temperatures into the metrics frame', () => {
        const state = getModularBridgeState()
        const frames: NodeItemMetrics[] = []
        const unsubscribe = subscribePush(event => {
            if (event.channel === 'metrics:update' && event.payload.id === 'uuid-temp-1') {
                frames.push(event.payload)
            }
        })
        try {
            state.handleNotification({
                source: 'broker',
                method: 'discovery:nodes-changed',
                params: {
                    nodes: [
                        {
                            hostUuid: 'uuid-temp-1',
                            name: 'temp-host-1',
                            ipAddress: '192.0.2.61',
                            port: 14318
                        }
                    ]
                }
            })
            state.mergeNodeInfoResponse('uuid-temp-1', {
                hostUuid: 'uuid-temp-1',
                GPUs: [
                    {
                        name: 'NVIDIA A',
                        vram_bytes: 1000,
                        vram_used_bytes: 10,
                        utilization_percent: 5,
                        temperature_celsius: 61
                    },
                    {
                        name: 'Edge TPU',
                        kind: 'npu',
                        utilization_percent: 30,
                        temperature_celsius: 52
                    }
                ],
                cpu: { name: 'CPU-A', cores: 8, utilization_percent: 3, temperature_celsius: 58 }
            })

            const topology = state.getNodesInitial().nodes['uuid-temp-1'].topology
            expect(topology.gpus).toHaveLength(2)
            expect(topology.gpus[0].kind).toBeUndefined()
            expect(topology.gpus[1]).toMatchObject({ name: 'Edge TPU', vramTotal: 0, kind: 'npu' })

            expect(frames.length).toBeGreaterThan(0)
            const latest = frames[frames.length - 1].current
            expect(latest.gpuTemperature).toEqual([
                { id: 'uuid-temp-1:gpu:0', value: 61 },
                { id: 'uuid-temp-1:gpu:1', value: 52 }
            ])
            expect(latest.cpuTemperature).toBe(58)

            // Only the temperatures move: that is still a telemetry change.
            const before = frames.length
            state.mergeNodeInfoResponse('uuid-temp-1', {
                hostUuid: 'uuid-temp-1',
                GPUs: [
                    {
                        name: 'NVIDIA A',
                        vram_bytes: 1000,
                        vram_used_bytes: 10,
                        utilization_percent: 5,
                        temperature_celsius: 63
                    },
                    {
                        name: 'Edge TPU',
                        kind: 'npu',
                        utilization_percent: 30,
                        temperature_celsius: 52
                    }
                ],
                cpu: { name: 'CPU-A', cores: 8, utilization_percent: 3, temperature_celsius: 58 }
            })
            expect(frames.length).toBeGreaterThan(before)
            expect(frames[frames.length - 1].current.gpuTemperature?.[0].value).toBe(63)
        } finally {
            unsubscribe()
        }
    })

    it('reports zero when a node omits every temperature', () => {
        const state = getModularBridgeState()
        const frames: NodeItemMetrics[] = []
        const unsubscribe = subscribePush(event => {
            if (event.channel === 'metrics:update' && event.payload.id === 'uuid-temp-2') {
                frames.push(event.payload)
            }
        })
        try {
            state.handleNotification({
                source: 'broker',
                method: 'discovery:nodes-changed',
                params: {
                    nodes: [
                        {
                            hostUuid: 'uuid-temp-2',
                            name: 'temp-host-2',
                            ipAddress: '192.0.2.62',
                            port: 14318
                        }
                    ]
                }
            })
            state.mergeNodeInfoResponse('uuid-temp-2', {
                hostUuid: 'uuid-temp-2',
                GPUs: [
                    {
                        name: 'NVIDIA B',
                        vram_bytes: 1000,
                        vram_used_bytes: 10,
                        utilization_percent: 5
                    }
                ],
                cpu: { name: 'CPU-B', cores: 4, utilization_percent: 1 }
            })
            const latest = frames[frames.length - 1].current
            expect(latest.gpuTemperature).toEqual([{ id: 'uuid-temp-2:gpu:0', value: 0 }])
            expect(latest.cpuTemperature).toBe(0)
        } finally {
            unsubscribe()
        }
    })
})
