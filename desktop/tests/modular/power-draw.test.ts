// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it, vi } from 'vitest'

vi.mock('electron', () => ({ BrowserWindow: { getAllWindows: () => [] } }))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))

import { getModularBridgeState } from '@/electron/service-bridge/modular-state'
import { subscribePush } from '@/electron/service-bridge/push-bus'
import type { NodeItemMetrics } from '@/shared/types/metrics'
import { cpuThermalSuffix, formatWatts, thermalLine } from '@/ui/utils/hardware-rows'

// node-info reports power_watts on the rows whose device meters itself — an
// NVIDIA card through nvidia-smi's power.draw, an AMD card through its hwmon
// PPT input, and the CPU through the processor's energy counter where that is
// readable. Everything else in a node's inventory has no meter at all, and
// the field is simply absent there.
//
// Both halves are asserted below, because the absent half is the one that can
// go wrong quietly: a 0 that reaches the card renders as a device drawing no
// power, which is a measurement nobody took.

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

describe('power draw', () => {
    it('carries GPU and CPU wattage into the metrics frame, and nothing for an unmetered row', () => {
        const state = getModularBridgeState()
        const frames: NodeItemMetrics[] = []
        const unsubscribe = subscribePush(event => {
            if (event.channel === 'metrics:update' && event.payload.id === 'uuid-power-1') {
                frames.push(event.payload)
            }
        })
        try {
            announce(state, 'uuid-power-1', '192.0.2.91')
            state.mergeNodeInfoResponse('uuid-power-1', {
                hostUuid: 'uuid-power-1',
                GPUs: [
                    {
                        name: 'NVIDIA GeForce RTX 5070 Ti',
                        vram_bytes: 17179869184,
                        vram_used_bytes: 2147483648,
                        utilization_percent: 41,
                        temperature_celsius: 38,
                        power_watts: 36
                    },
                    // Metered but with no thermal readout: the wattage must
                    // survive on its own.
                    {
                        name: 'AMD Radeon Vega Graphics (Barcelo, GCN 5.1)',
                        vram_bytes: 16776339456,
                        vram_used_bytes: 54980608,
                        memory_pool: 'unified',
                        power_watts: 21
                    },
                    // No meter at all: the field never arrives.
                    { name: 'Google Coral Edge TPU', kind: 'npu', temperature_celsius: 52 }
                ],
                cpu: {
                    name: 'Intel(R) Core(TM) i9-10980XE',
                    cores: 18,
                    utilization_percent: 12,
                    temperature_celsius: 63,
                    power_watts: 79
                }
            })

            expect(frames.length).toBeGreaterThan(0)
            const latest = frames[frames.length - 1].current
            expect(latest.gpuPower).toEqual([
                { id: gpuId('uuid-power-1', 0), value: 36 },
                { id: gpuId('uuid-power-1', 1), value: 21 },
                { id: gpuId('uuid-power-1', 2), value: 0 }
            ])
            expect(latest.cpuPower).toBe(79)

            // A wattage that moves while nothing else does is still new
            // telemetry — otherwise a GPU ramping up would never repaint.
            const before = frames.length
            state.mergeNodeInfoResponse('uuid-power-1', {
                hostUuid: 'uuid-power-1',
                GPUs: [
                    {
                        name: 'NVIDIA GeForce RTX 5070 Ti',
                        vram_bytes: 17179869184,
                        vram_used_bytes: 2147483648,
                        utilization_percent: 41,
                        temperature_celsius: 38,
                        power_watts: 210
                    }
                ],
                cpu: {
                    name: 'Intel(R) Core(TM) i9-10980XE',
                    cores: 18,
                    utilization_percent: 12,
                    temperature_celsius: 63,
                    power_watts: 79
                }
            })
            expect(frames.length).toBeGreaterThan(before)
            expect(frames[frames.length - 1].current.gpuPower?.[0].value).toBe(210)
        } finally {
            unsubscribe()
        }
    })

    it('reports zero when a node omits every wattage', () => {
        const state = getModularBridgeState()
        const frames: NodeItemMetrics[] = []
        const unsubscribe = subscribePush(event => {
            if (event.channel === 'metrics:update' && event.payload.id === 'uuid-power-2') {
                frames.push(event.payload)
            }
        })
        try {
            announce(state, 'uuid-power-2', '192.0.2.92')
            state.mergeNodeInfoResponse('uuid-power-2', {
                hostUuid: 'uuid-power-2',
                GPUs: [{ name: 'NVIDIA A2', vram_bytes: 1000, utilization_percent: 5 }],
                cpu: { name: 'CPU', cores: 4, utilization_percent: 1 }
            })
            const latest = frames[frames.length - 1].current
            expect(latest.gpuPower).toEqual([{ id: gpuId('uuid-power-2', 0), value: 0 }])
            expect(latest.cpuPower).toBe(0)
        } finally {
            unsubscribe()
        }
    })

    it('renders the wattage beside the temperature, and alone when there is none', () => {
        // The shape the operator asked for: one line per device state.
        expect(thermalLine(50, 200)).toEqual({ label: 'Temp', value: '50 °C (200 W)' })
        // A card with a thermal readout and no meter is unchanged.
        expect(thermalLine(64, 0)).toEqual({ label: 'Temp', value: '64 °C' })
        // Metered with no thermal readout — an AMD APU row — keeps its figure
        // instead of losing it with the temperature it would have hung off.
        expect(thermalLine(0, 21)).toEqual({ label: 'Power', value: '21 W' })
        // Neither: no row at all rather than a placeholder.
        expect(thermalLine(0, 0)).toBeNull()

        // Whole watts; the services publish whole watts and a decimal here
        // would imply a precision the meters are not reporting.
        expect(formatWatts(35.65)).toBe('36 W')
        expect(formatWatts(0)).toBe('')
        expect(formatWatts(-1)).toBe('')
    })

    it('appends the same pair to the CPU summary line', () => {
        expect(cpuThermalSuffix(55, 140)).toBe(' · 55 °C (140 W)')
        expect(cpuThermalSuffix(55, 0)).toBe(' · 55 °C')
        expect(cpuThermalSuffix(0, 140)).toBe(' · 140 W')
        // A Linux host, where the energy counter is root-only and this
        // service runs unprivileged: neither figure, so no suffix.
        expect(cpuThermalSuffix(0, 0)).toBe('')
    })
})
