// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Which lines a hardware row in the node card is allowed to show.
 *
 * The inventory a node publishes is no longer only GPUs. It also carries
 * inference accelerators (`kind: "npu"`) and the host's motherboard controller
 * (`kind: "board"`), and each of those genuinely cannot report some of what a
 * GPU reports. A row that prints "Usage 0%" for a device with no busy counter
 * is not a neutral placeholder: it reads as "idle", which is a claim, and a
 * wrong one.
 *
 * The same applies to what a line SAYS. Several devices here have no memory of
 * their own — an integrated GPU, an Arm Mali GPU, an RKNPU, an Apple Silicon
 * GPU — and their capacity is a pool shared with the CPU. Calling that "VRAM"
 * claims the device owns memory it does not, and pairing it with the host's
 * usage claimed it was spending memory it never touched: that is how an idle
 * Intel UHD Graphics 630 came to be displayed as "VRAM 25.5 GB / 66 GB".
 *
 * So the rule is per line and per row, and it lives here rather than in each
 * component, because the legend and the performance panel have to agree — a
 * row that shows Usage in one and not the other is worse than either.
 */

import { MEMORY_POOL_UNIFIED } from '@/shared/constants/hardware'
import { formatBytes } from '@/ui/utils/formatters'

/** The minimum shape both the legend and the performance panel share. */
export interface HardwareRow {
    /** "" or undefined for a GPU, "npu" for an accelerator, "board" for the motherboard. */
    kind?: string
    /** Total VRAM in bytes; 0 for a device that has none. */
    vramTotal: number
    /**
     * MEMORY_POOL_UNIFIED when vramTotal is a pool shared with the host rather
     * than the device's own memory; absent on a discrete card.
     */
    memoryPool?: string
}

/** The motherboard controller row's kind, as node-info spells it. */
export const KIND_BOARD = 'board'

/** The inference-accelerator row's kind, as node-info spells it. */
export const KIND_ACCELERATOR = 'npu'

/**
 * Whether a row may show a Usage line.
 *
 * False for the motherboard controller: ASUS's TPU and EPU — and the
 * equivalents other vendors fit — publish no load or status interface, so
 * node-info deliberately sends no utilization_percent for that row and the UI
 * must not invent a zero for it.
 */
export function showsUsage(row: HardwareRow): boolean {
    return row.kind !== KIND_BOARD
}

/**
 * Whether a row may show a VRAM line.
 *
 * False for the motherboard controller, which has no memory at all, and for an
 * accelerator that reported none — that row would only ever read "0 B / 0 B".
 */
export function showsVram(row: HardwareRow): boolean {
    if (row.kind === KIND_BOARD) return false
    return !(row.kind === KIND_ACCELERATOR && row.vramTotal === 0)
}

/**
 * Whether this row's capacity is a pool it shares with the host.
 *
 * True for an integrated GPU, an Arm Mali GPU, an RKNPU, an Apple Silicon GPU
 * and an NVIDIA UMA part. The bytes are real and reachable, but they are also
 * the CPU's, so the row must not present them as memory set aside for itself.
 */
export function isUnifiedMemory(row: HardwareRow): boolean {
    return row.memoryPool === MEMORY_POOL_UNIFIED
}

/**
 * The label for a row's memory line: "VRAM" for dedicated device memory,
 * "Shared" for a pool the device and the host draw from together.
 */
export function memoryLabel(row: HardwareRow): string {
    return isUnifiedMemory(row) ? 'Shared' : 'VRAM'
}

/**
 * Whether a row shows a memory line at all, and — as a type predicate — that
 * the used figure passed in is a real number when it does.
 *
 * Two rules, in order. A device with no memory to speak of never had one: the
 * motherboard controller, and an accelerator that reported no capacity.
 *
 * The second rule is newer, and it retires the "Shared <total>" line. A
 * shared-pool device that measures nothing — an Intel iGPU, a Mali GPU, an
 * RKNPU — publishes only a ceiling, and a ceiling on its own is a static
 * number that never moves: it sits among live readings looking like one,
 * while saying nothing about what the device is doing. So such a row now
 * shows usage and temperature and no memory line, and a shared pool the
 * device DOES measure (an AMD APU, an Apple Silicon GPU, a DGX Spark part)
 * keeps the full "Shared <used> / <total>". Dedicated rows are untouched:
 * their used figure is always a number, 0 included, which is a real reading.
 */
export function showsMemoryLine(row: HardwareRow, usedBytes: number | null): usedBytes is number {
    return usedBytes !== null && showsVram(row)
}

/**
 * The value beside that label: "<used> / <total>".
 *
 * Only ever called for a row that has a used figure — see showsMemoryLine,
 * which is the guard that establishes it.
 */
export function memoryLineValue(row: HardwareRow, usedBytes: number): string {
    return `${formatBytes(usedBytes, 1)} / ${formatBytes(row.vramTotal, 1)}`
}

/**
 * The thermal/power line for a device row, or null when it reports neither.
 *
 * Temperature leads and power rides in parentheses beside it — "50 °C (200 W)"
 * — because they describe the same thing and two separate rows for one device
 * state reads as two measurements. A device that is metered but has no thermal
 * readout gets a line of its own instead, so the wattage is not lost with the
 * temperature it would have hung off.
 *
 * Zero means "nothing measured this" on both, never "cold" or "drawing
 * nothing": node-info omits either field wherever it has no source, and a
 * literal 0 arriving here is that omission (see shared/types/metrics.ts).
 */
export function thermalLine(
    temperatureC: number,
    powerWatts: number
): { label: string; value: string } | null {
    const watts = formatWatts(powerWatts)
    if (temperatureC > 0) {
        return {
            label: 'Temp',
            value: watts ? `${temperatureC} °C (${watts})` : `${temperatureC} °C`
        }
    }
    return watts ? { label: 'Power', value: watts } : null
}

/**
 * Whole watts with a unit, or "" when there is no reading. Rounded because
 * the services publish whole watts and a decimal here would only imply a
 * precision the meters are not reporting.
 */
export function formatWatts(powerWatts: number): string {
    return powerWatts > 0 ? `${Math.round(powerWatts)} W` : ''
}

/**
 * The trailing "· 55 °C (140 W)" on the CPU's single summary line, or "" when
 * the host reports neither. Same precedence as thermalLine, flattened into
 * one line because the CPU has one.
 */
export function cpuThermalSuffix(temperatureC: number, powerWatts: number): string {
    const line = thermalLine(temperatureC, powerWatts)
    return line ? ` · ${line.value}` : ''
}

/**
 * The used-bytes figure for a row, from its chart series' latest percentage,
 * or null when there is no figure to show.
 *
 * The bridge emits no VRAM series for a unified row the node did not measure
 * (see reportsMemoryUsage in electron/service-bridge/modular-state.ts), so a
 * missing series on such a row means "unmeasured" and yields null — which
 * showsMemoryLine then turns into no memory line at all. On a row with
 * dedicated memory a missing series only ever means "no sample yet", and it
 * keeps reading 0 the way it always has rather than losing its line.
 */
export function memoryUsedBytes(row: HardwareRow, usagePercent: number | null): number | null {
    if (usagePercent === null) return isUnifiedMemory(row) ? null : 0
    return Math.floor((row.vramTotal * usagePercent) / 100)
}
