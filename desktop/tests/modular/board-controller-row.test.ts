// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it, vi } from 'vitest'

vi.mock('electron', () => ({ BrowserWindow: { getAllWindows: () => [] } }))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))

import { getModularBridgeState } from '@/electron/service-bridge/modular-state'
import { subscribePush } from '@/electron/service-bridge/push-bus'
import type { NodeItemMetrics } from '@/shared/types/metrics'
import { KIND_BOARD, showsUsage, showsVram } from '@/ui/utils/hardware-rows'

// node-info lists the host's motherboard controller beside the GPUs, as a row
// with kind "board" that carries a temperature and nothing else. Two things
// have to hold for it to arrive intact and read honestly: the bridge has to
// carry a kind it was not written for all the way through, and the UI must not
// print the figures that row has no way to report.
describe('motherboard controller row', () => {
    it('carries a kind:"board" row and its temperature through the bridge', () => {
        const state = getModularBridgeState()
        const frames: NodeItemMetrics[] = []
        const unsubscribe = subscribePush(event => {
            if (event.channel === 'metrics:update' && event.payload.id === 'uuid-board-1') {
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
                            hostUuid: 'uuid-board-1',
                            name: 'board-host-1',
                            ipAddress: '192.0.2.71',
                            port: 14318
                        }
                    ]
                }
            })
            state.mergeNodeInfoResponse('uuid-board-1', {
                hostUuid: 'uuid-board-1',
                GPUs: [
                    {
                        name: 'NVIDIA GeForce RTX 5060 Ti',
                        vram_bytes: 17179869184,
                        vram_used_bytes: 2147483648,
                        utilization_percent: 41,
                        temperature_celsius: 55
                    },
                    {
                        name: 'ROG Dual Intelligent Processors',
                        kind: 'board',
                        temperature_celsius: 44
                    }
                ]
            })

            const topology = state.getNodesInitial().nodes['uuid-board-1'].topology
            expect(topology.gpus).toHaveLength(2)
            // A controller has no memory and no busy counter, so the row must
            // not acquire either on the way through.
            expect(topology.gpus[1]).toMatchObject({
                name: 'ROG Dual Intelligent Processors',
                vramTotal: 0,
                kind: KIND_BOARD
            })

            // The temperature travels on the metrics channel keyed by the same
            // index the topology row got; that join is what puts the degrees on
            // the right row.
            expect(frames.length).toBeGreaterThan(0)
            const latest = frames[frames.length - 1].current
            expect(latest.gpuTemperature).toEqual([
                { id: 'uuid-board-1:gpu:0', value: 55 },
                { id: 'uuid-board-1:gpu:1', value: 44 }
            ])
            expect(topology.gpus[1].id).toBe('uuid-board-1:gpu:1')
        } finally {
            unsubscribe()
        }
    })

    it('treats a board-temperature-only change as new telemetry', () => {
        // Without this the row would freeze at whatever it first reported: the
        // bridge skips the upsert when nothing looks different.
        const state = getModularBridgeState()
        const seed = {
            hostUuid: 'uuid-board-2',
            GPUs: [
                { name: 'ROG Dual Intelligent Processors', kind: 'board', temperature_celsius: 41 }
            ]
        }
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: {
                nodes: [
                    {
                        hostUuid: 'uuid-board-2',
                        name: 'board-host-2',
                        ipAddress: '192.0.2.72',
                        port: 14318
                    }
                ]
            }
        })
        state.mergeNodeInfoResponse('uuid-board-2', seed)
        state.mergeNodeInfoResponse('uuid-board-2', {
            ...seed,
            GPUs: [
                { name: 'ROG Dual Intelligent Processors', kind: 'board', temperature_celsius: 46 }
            ]
        })

        const gpus = state.getAvailableNodes().find(n => n.id === 'uuid-board-2')
        expect(gpus).toBeDefined()
        const topology = state.getNodesInitial().nodes['uuid-board-2'].topology
        expect(topology.gpus[0].kind).toBe(KIND_BOARD)
    })

    it('shows neither Usage nor VRAM for a board row, and both for a GPU', () => {
        const board = { kind: KIND_BOARD, vramTotal: 0 }
        expect(showsUsage(board)).toBe(false)
        expect(showsVram(board)).toBe(false)

        const gpu = { vramTotal: 17179869184 }
        expect(showsUsage(gpu)).toBe(true)
        expect(showsVram(gpu)).toBe(true)
    })

    it('leaves the accelerator rule alone: Usage shows, VRAM only when it has some', () => {
        // An Edge TPU / NPU does report a busy figure, so its Usage line stays;
        // that is the behaviour the board row must not have changed.
        expect(showsUsage({ kind: 'npu', vramTotal: 0 })).toBe(true)
        expect(showsVram({ kind: 'npu', vramTotal: 0 })).toBe(false)
        expect(showsVram({ kind: 'npu', vramTotal: 4294967296 })).toBe(true)
    })
})
