// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'

import type { WorkloadState } from '@/shared/types/workloads'
import { parseWorkloadRequester, workloadNodeRowLabel } from '@/shared/utils/workloads'

const STARTED = 1_700_000_000_000

function label(state: WorkloadState, startedAt: number | null): string {
    return workloadNodeRowLabel({ state, startedAt })
}

describe('workloadNodeRowLabel', () => {
    it('says what each state did on the node', () => {
        expect(label('running', STARTED)).toBe('Running on')
        expect(label('initializing', null)).toBe('Starting on')
        expect(label('queued', null)).toBe('Queued on')
        expect(label('completed', STARTED)).toBe('Ran on')
    })

    it('does not claim a failed job ran when it never started', () => {
        expect(label('failed', STARTED)).toBe('Ran on')
        expect(label('failed', null)).toBe('Never started on')
    })

    it('treats startedAt of 0 as set (only null means never started)', () => {
        expect(label('failed', 0)).toBe('Ran on')
    })
})

describe('parseWorkloadRequester', () => {
    const uuid = '3f2a9c1e-7b4d-4e8a-9f60-1a2b3c4d5e6f'

    it('returns null for empty, whitespace, null and bare harness ids', () => {
        expect(parseWorkloadRequester(null)).toBeNull()
        expect(parseWorkloadRequester(undefined)).toBeNull()
        expect(parseWorkloadRequester('')).toBeNull()
        expect(parseWorkloadRequester('   ')).toBeNull()
        expect(parseWorkloadRequester('offload-harness')).toBeNull()
        expect(parseWorkloadRequester('offload-harness/')).toBeNull()
    })

    it('reads a fleet asker, with or without the harness prefix and a trailing path', () => {
        expect(parseWorkloadRequester('fleet:node-a')).toEqual({ kind: 'fleet', asker: 'node-a' })
        expect(parseWorkloadRequester('offload-harness/fleet:node-a')).toEqual({
            kind: 'fleet',
            asker: 'node-a'
        })
        expect(parseWorkloadRequester('offload-harness/fleet:node-a/agent_delegate/7')).toEqual({
            kind: 'fleet',
            asker: 'node-a'
        })
        expect(parseWorkloadRequester('fleet:  spaced name  /x')).toEqual({
            kind: 'fleet',
            asker: 'spaced name'
        })
    })

    it('caps the fleet asker at 64 characters', () => {
        const parsed = parseWorkloadRequester(`fleet:${'a'.repeat(100)}`)
        expect(parsed).toEqual({ kind: 'fleet', asker: 'a'.repeat(64) })
    })

    it('falls through to other when the fleet asker is empty', () => {
        expect(parseWorkloadRequester('fleet:')).toEqual({ kind: 'other', label: 'fleet:' })
        expect(parseWorkloadRequester('fleet:/x')).toEqual({ kind: 'other', label: 'fleet:/x' })
    })

    it('shortens a session UUID to its first 8 hex digits', () => {
        const expected = { kind: 'session', label: 'session 3f2a9c1e' }
        expect(parseWorkloadRequester(uuid)).toEqual(expected)
        expect(parseWorkloadRequester(`offload-harness/${uuid}`)).toEqual(expected)
        expect(parseWorkloadRequester(uuid.toUpperCase())).toEqual(expected)
    })

    it('keeps any other id verbatim', () => {
        expect(parseWorkloadRequester('curl-smoke')).toEqual({ kind: 'other', label: 'curl-smoke' })
        expect(parseWorkloadRequester('offload-harness/some-tool')).toEqual({
            kind: 'other',
            label: 'some-tool'
        })
        // Not a harness namespace: the word merely starts the id.
        expect(parseWorkloadRequester('offload-harnessed')).toEqual({
            kind: 'other',
            label: 'offload-harnessed'
        })
    })

    it('truncates a long other id to 48 characters including the ellipsis', () => {
        const exact = 'b'.repeat(48)
        expect(parseWorkloadRequester(exact)).toEqual({ kind: 'other', label: exact })

        const parsed = parseWorkloadRequester('c'.repeat(80))
        expect(parsed).toEqual({ kind: 'other', label: `${'c'.repeat(47)}…` })
        expect(parsed?.kind === 'other' && parsed.label.length).toBe(48)
    })
})
