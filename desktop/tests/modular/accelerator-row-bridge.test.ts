// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it, vi } from 'vitest'

vi.mock('electron', () => ({ BrowserWindow: { getAllWindows: () => [] } }))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))

import { getModularBridgeState } from '@/electron/service-bridge/modular-state'
import { subscribePush } from '@/electron/service-bridge/push-bus'
import type { NodeItemMetrics } from '@/shared/types/metrics'
import { KIND_ACCELERATOR, showsVram } from '@/ui/utils/hardware-rows'

// node-info lists inference accelerators beside the GPUs, as rows with a
// kind. The bridge has to carry that kind all the way through, and a row that
// carries only a temperature must still repaint when that temperature moves.
describe('non-GPU rows through the bridge', () => {
    it('carries a kind:"npu" row and its temperature through the bridge', () => {
        const state = getModularBridgeState()
        const frames: NodeItemMetrics[] = []
        const unsubscribe = subscribePush(event => {
            if (event.channel === 'metrics:update' && event.payload.id === 'uuid-accel-1') {
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
                            hostUuid: 'uuid-accel-1',
                            name: 'accel-host-1',
                            ipAddress: '192.0.2.71',
                            port: 14318
                        }
                    ]
                }
            })
            state.mergeNodeInfoResponse('uuid-accel-1', {
                hostUuid: 'uuid-accel-1',
                GPUs: [
                    {
                        name: 'NVIDIA GeForce RTX 5060 Ti',
                        vram_bytes: 17179869184,
                        vram_used_bytes: 2147483648,
                        utilization_percent: 41,
                        temperature_celsius: 55
                    },
                    {
                        name: 'Hailo-8L',
                        kind: 'npu',
                        temperature_celsius: 44
                    }
                ]
            })

            const topology = state.getNodesInitial().nodes['uuid-accel-1'].topology
            expect(topology.gpus).toHaveLength(2)
            // A module with no memory figure must not acquire one on the way.
            expect(topology.gpus[1]).toMatchObject({
                name: 'Hailo-8L',
                vramTotal: 0,
                kind: KIND_ACCELERATOR
            })

            // The temperature travels on the metrics channel keyed by the same
            // index the topology row got; that join is what puts the degrees on
            // the right row.
            expect(frames.length).toBeGreaterThan(0)
            const latest = frames[frames.length - 1].current
            expect(latest.gpuTemperature).toEqual([
                { id: 'uuid-accel-1:gpu:0', value: 55 },
                { id: 'uuid-accel-1:gpu:1', value: 44 }
            ])
            expect(topology.gpus[1].id).toBe('uuid-accel-1:gpu:1')
        } finally {
            unsubscribe()
        }
    })

    it('treats a temperature-only change as new telemetry', () => {
        // Without this the row would freeze at whatever it first reported: the
        // bridge skips the upsert when nothing looks different.
        const state = getModularBridgeState()
        const seed = {
            hostUuid: 'uuid-accel-2',
            GPUs: [{ name: 'Hailo-8L', kind: 'npu', temperature_celsius: 41 }]
        }
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: {
                nodes: [
                    {
                        hostUuid: 'uuid-accel-2',
                        name: 'accel-host-2',
                        ipAddress: '192.0.2.72',
                        port: 14318
                    }
                ]
            }
        })
        state.mergeNodeInfoResponse('uuid-accel-2', seed)
        state.mergeNodeInfoResponse('uuid-accel-2', {
            ...seed,
            GPUs: [{ name: 'Hailo-8L', kind: 'npu', temperature_celsius: 46 }]
        })

        const gpus = state.getAvailableNodes().find(n => n.id === 'uuid-accel-2')
        expect(gpus).toBeDefined()
        const topology = state.getNodesInitial().nodes['uuid-accel-2'].topology
        expect(topology.gpus[0].kind).toBe(KIND_ACCELERATOR)
    })

    it('shows an accelerator VRAM line only when it has some', () => {
        expect(showsVram({ kind: 'npu', vramTotal: 0 })).toBe(false)
        expect(showsVram({ kind: 'npu', vramTotal: 4294967296 })).toBe(true)
    })
})
