// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import type { AvailableNode, ClusterNode } from '@/shared/types/cluster'
import type { NodeItem } from '@/shared/types/nodes'
import { filterInvitablePeers } from '@/ui/utils/invitable-peers'
import { nodeCardControls } from '@/ui/utils/node-card-controls'
import { buildOverviewNodes } from '@/ui/utils/overview-nodes'

const SELF = 'self-uuid'
const MEMBER = 'member-uuid'
const VIEW_ONLY = 'view-only-uuid'
const STRANGER = 'stranger-uuid'

function nodeItem(id: string, overrides: Partial<NodeItem> = {}): NodeItem {
    return {
        id,
        name: id,
        status: 'active',
        ipAddress: '192.0.2.1',
        port: 14318,
        allIpAddresses: ['192.0.2.1'],
        topology: { cpu: { model: 'CPU', cores: 8, threads: 8 }, gpus: [], ram: 0, storage: [] },
        os: 'Windows',
        ...overrides
    }
}

function member(nodeUuid: string): ClusterNode {
    return {
        id: nodeUuid,
        nodeUuid,
        name: nodeUuid,
        ipAddress: '192.0.2.2',
        port: 14318,
        clusterId: 'cluster-1',
        state: 'member',
        joinedAt: 1,
        lastSeen: 1
    }
}

function available(id: string): AvailableNode {
    return {
        id,
        name: id,
        ipAddress: '192.0.2.3',
        port: 14318,
        lastSeen: 1,
        trusted: false,
        clustered: false
    }
}

function overviewIds(nodes: NodeItem[]): string[] {
    return buildOverviewNodes(
        new Map(nodes.map(node => [node.id, node])),
        [member(SELF), member(MEMBER)],
        SELF,
        'Windows'
    ).map(node => node.id)
}

describe('overview nodes with a view-only node', () => {
    it('includes a view-only node', () => {
        const ids = overviewIds([
            nodeItem(SELF),
            nodeItem(MEMBER),
            nodeItem(VIEW_ONLY, { viewOnly: true })
        ])

        expect(ids).toContain(VIEW_ONLY)
    })

    it('lists a view-only node after the members', () => {
        const ids = overviewIds([
            nodeItem(VIEW_ONLY, { viewOnly: true }),
            nodeItem(MEMBER),
            nodeItem(SELF)
        ])

        expect(ids).toEqual([SELF, MEMBER, VIEW_ONLY])
    })

    it('keeps the view-only flag on the card', () => {
        const nodes = buildOverviewNodes(
            new Map([[VIEW_ONLY, nodeItem(VIEW_ONLY, { viewOnly: true })]]),
            [member(SELF)],
            SELF,
            'Windows'
        )

        expect(nodes.find(node => node.id === VIEW_ONLY)?.viewOnly).toBe(true)
    })

    it('still leaves out a discovered node that is neither a member nor view-only', () => {
        const ids = overviewIds([nodeItem(SELF), nodeItem(STRANGER)])

        expect(ids).not.toContain(STRANGER)
    })

    it('shows a view-only node when nothing else is known', () => {
        const nodes = buildOverviewNodes(
            new Map([[VIEW_ONLY, nodeItem(VIEW_ONLY, { viewOnly: true })]]),
            [],
            null,
            'Windows'
        )

        expect(nodes.map(node => node.id)).toEqual([VIEW_ONLY])
    })
})

describe('invitable peers with a view-only node', () => {
    it('offers an ordinary discovered peer', () => {
        const peers = filterInvitablePeers([available(STRANGER)], [], SELF, new Map())

        expect(peers.map(peer => peer.id)).toEqual([STRANGER])
    })

    it('leaves out a view-only node', () => {
        const nodesMap = new Map([[VIEW_ONLY, nodeItem(VIEW_ONLY, { viewOnly: true })]])

        const peers = filterInvitablePeers(
            [available(VIEW_ONLY), available(STRANGER)],
            [],
            SELF,
            nodesMap
        )

        expect(peers.map(peer => peer.id)).toEqual([STRANGER])
    })

    it('still leaves out members and this machine', () => {
        const peers = filterInvitablePeers(
            [available(MEMBER), available(SELF), available(STRANGER)],
            [member(MEMBER)],
            SELF,
            new Map()
        )

        expect(peers.map(peer => peer.id)).toEqual([STRANGER])
    })
})

describe('node card controls', () => {
    it('offers engine toggles on an ordinary node', () => {
        expect(nodeCardControls(nodeItem(MEMBER)).engineToggles).toBe(true)
    })

    it('offers engine settings on an ordinary node', () => {
        expect(nodeCardControls(nodeItem(MEMBER)).engineSettings).toBe(true)
    })

    it('offers the performance chart on an ordinary node', () => {
        expect(nodeCardControls(nodeItem(MEMBER)).performanceChart).toBe(true)
    })

    it('offers no engine toggles on a view-only node', () => {
        expect(nodeCardControls(nodeItem(VIEW_ONLY, { viewOnly: true })).engineToggles).toBe(false)
    })

    it('offers no engine settings on a view-only node', () => {
        expect(nodeCardControls(nodeItem(VIEW_ONLY, { viewOnly: true })).engineSettings).toBe(false)
    })

    it('offers no buttons at all on a view-only node', () => {
        const controls = nodeCardControls(nodeItem(VIEW_ONLY, { viewOnly: true }))

        expect(Object.values(controls).every(offered => offered === false)).toBe(true)
    })
})
