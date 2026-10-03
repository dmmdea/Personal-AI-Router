// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { Workload } from '@/shared/types/workloads'

/**
 * Stable catalog key for a workload.
 *
 * The backend's catalog is keyed by `(originatedFrom, engine, runId, id)`, but
 * the `workloads:remove` push carries only `(workloadId, originatedFrom)` — and
 * the broker's own `Store.Remove` drops every record matching that pair — so
 * `(originatedFrom, id)` is the only key a subscribe client can maintain
 * consistently across upsert and remove. Each node's proxy assigns workload ids
 * from its own monotonic counter, so ids collide across nodes; `originatedFrom`
 * (the origin node) disambiguates. Mirror that here so a remote node's job never
 * overwrites a local one that happens to share an id. The `\u0000` separator
 * cannot appear in a host id or proxy counter, so the key is unambiguous.
 */
export function workloadKey(originatedFrom: string | null, id: string): string {
    return `${originatedFrom ?? ''}\u0000${id}`
}

/**
 * Which node a workload actually ran on, for UI attribution.
 *
 * `scheduledOn` is the node the scheduler routed the request to (where it
 * actually runs); `originatedFrom` is only where the request entered the
 * cluster. UI that means "which node is this job on" (per-node job badges, the
 * workload→node connection lines, the "ran on" label) attributes strictly to
 * `scheduledOn`. A `null` result means the workload has **not been scheduled
 * yet** (still queued) — it has no run-node, so it draws no connection line, is
 * counted on no node, and shows no "ran on" target. Deliberately does NOT fall
 * back to `originatedFrom`: the origin is where the request came from, not where
 * the job ran.
 */
export function workloadExecutionNodeId(workload: Pick<Workload, 'scheduledOn'>): string | null {
    return workload.scheduledOn ?? null
}

/**
 * Label for the "which node" row on a job card, matched to the job's state so a
 * job that never ran is not described as having run.
 *
 * `workloadExecutionNodeId` only supplies the node name; the verb has to say
 * what happened there. A failed job with no `startedAt` was scheduled (or
 * failed before it began) but never executed, so it reads "Never started on".
 */
export function workloadNodeRowLabel(workload: Pick<Workload, 'state' | 'startedAt'>): string {
    switch (workload.state) {
        case 'running':
            return 'Running on'
        case 'initializing':
            return 'Starting on'
        case 'queued':
            return 'Queued on'
        case 'completed':
            return 'Ran on'
        case 'failed':
            return workload.startedAt != null ? 'Ran on' : 'Never started on'
    }
}

/** Who asked for a workload, parsed from the backend's free-form `requesterId`. */
export type WorkloadRequester =
    | { kind: 'fleet'; asker: string }
    | { kind: 'session'; label: string }
    | { kind: 'other'; label: string }

const REQUESTER_HARNESS_PREFIX = /^offload-harness(?:\/|$)/
const REQUESTER_FLEET_PREFIX = 'fleet:'
const REQUESTER_SESSION_UUID =
    /^([0-9a-f]{8})-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}(?:\/|$)/i
const REQUESTER_ASKER_MAX = 64
const REQUESTER_LABEL_MAX = 48

/**
 * Parse a workload's `requesterId` into something a person can read.
 *
 * A leading `offload-harness` segment (and the one `/` after it) is the harness
 * namespace and carries no information, so it is dropped. What remains is one of:
 * `fleet:<asker>[/...]` (a fleet seat asking on someone's behalf, shown by the
 * asker's name), a session UUID (shown as `session <first 8 hex>`), or anything
 * else (shown verbatim, truncated to 48 chars with an ellipsis). Empty or
 * missing ids yield `null` so the card draws nothing extra.
 */
export function parseWorkloadRequester(
    requesterId: string | null | undefined
): WorkloadRequester | null {
    if (requesterId == null) return null
    const rest = requesterId.trim().replace(REQUESTER_HARNESS_PREFIX, '').trim()
    if (rest === '') return null

    if (rest.startsWith(REQUESTER_FLEET_PREFIX)) {
        const asker = rest
            .slice(REQUESTER_FLEET_PREFIX.length)
            .split('/')[0]
            .trim()
            .slice(0, REQUESTER_ASKER_MAX)
        if (asker !== '') return { kind: 'fleet', asker }
    }

    const uuid = REQUESTER_SESSION_UUID.exec(rest)
    if (uuid) return { kind: 'session', label: `session ${uuid[1].toLowerCase()}` }

    const label =
        rest.length > REQUESTER_LABEL_MAX ? `${rest.slice(0, REQUESTER_LABEL_MAX - 1)}…` : rest
    return { kind: 'other', label }
}
