// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { Workload } from '@/shared/types/workloads'

type LabelFields = Pick<Workload, 'state' | 'startedAt'> & Partial<Pick<Workload, 'error'>>

/**
 * A job that closed `completed` without starting, with its reason in `error`:
 * the producer held it back (another job held what it needed) or the node
 * handed it back, so nothing ran. PAIR has no "did not run" state, and the
 * offload harness closes such a call `completed` rather than `failed` so it
 * does not read as a fault.
 */
export function isHeldBack(workload: LabelFields): boolean {
    return workload.state === 'completed' && workload.startedAt === null && Boolean(workload.error)
}

/**
 * The verb before a job card's execution node. A job that ends before it
 * starts keeps the node it was sent to, so whether it ran is decided by
 * `startedAt`, not by its state, with one exception: a `completed` job with no
 * reason attached did run, even when its producer never stamped a start (a
 * short job that finished between two progress reports), and labelling it
 * "Sent to" read as work that never happened.
 */
export function workloadNodeLabel(workload: LabelFields): string {
    if (workload.state === 'running') return 'Running on'
    if (workload.startedAt !== null) return 'Ran on'
    return workload.state === 'completed' && !isHeldBack(workload) ? 'Ran on' : 'Sent to'
}
