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
 * The value beside that label: "<used> / <total>" when the node reported a
 * used figure, and "<total>" alone when it did not.
 *
 * The second form is the whole point. A shared-pool device whose driver counts
 * nothing (an Intel iGPU, a Mali GPU, an RKNPU) sends a ceiling and no usage,
 * and the honest rendering of that is the ceiling — not "0 B / 66 GB", which
 * asserts an idle GPU, and not the host's own usage, which asserts a busy one.
 * A device that DOES measure itself (an AMD APU, an Apple Silicon GPU, a DGX
 * Spark part) keeps the full form, still labelled "Shared".
 */
export function memoryLineValue(row: HardwareRow, usedBytes: number | null): string {
    const total = formatBytes(row.vramTotal, 1)
    return usedBytes === null ? total : `${formatBytes(usedBytes, 1)} / ${total}`
}

/**
 * The used-bytes figure for a row, from its chart series' latest percentage,
 * or null when there is no figure to show.
 *
 * The bridge emits no VRAM series for a unified row the node did not measure
 * (see reportsMemoryUsage in electron/service-bridge/modular-state.ts), so a
 * missing series on such a row means "unmeasured" and yields null. On a row
 * with dedicated memory a missing series only ever means "no sample yet", and
 * it keeps reading 0 the way it always has rather than losing its line.
 */
export function memoryUsedBytes(row: HardwareRow, usagePercent: number | null): number | null {
    if (usagePercent === null) return isUnifiedMemory(row) ? null : 0
    return Math.floor((row.vramTotal * usagePercent) / 100)
}
