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
 * So the rule is per line and per row, and it lives here rather than in each
 * component, because the legend and the performance panel have to agree — a
 * row that shows Usage in one and not the other is worse than either.
 */

/** The minimum shape both the legend and the performance panel share. */
export interface HardwareRow {
    /** "" or undefined for a GPU, "npu" for an accelerator, "board" for the motherboard. */
    kind?: string
    /** Total VRAM in bytes; 0 for a device that has none. */
    vramTotal: number
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
