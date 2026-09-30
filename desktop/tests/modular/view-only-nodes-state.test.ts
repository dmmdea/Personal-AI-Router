// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

vi.mock('electron', () => ({ BrowserWindow: { getAllWindows: () => [] } }))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))

const mocks = vi.hoisted(() => ({
    warnings: [] as string[]
}))

vi.mock('@/shared/utils/log', () => ({
    createStructuredLogger: () => ({
        info: (): void => {},
        warn: (payload: { message: string }): void => {
            mocks.warnings.push(payload.message)
        },
        error: (): void => {},
        verbose: (): void => {}
    })
}))

import { VIEW_ONLY_MAX_GPUS, getModularBridgeState } from '@/electron/service-bridge/modular-state'
import { startNodeInfoPoller, stopNodeInfoPoller } from '@/electron/service-bridge/node-info-poller'
import { subscribePush } from '@/electron/service-bridge/push-bus'
import type { ViewOnlyNodeEntry } from '@/electron/service-bridge/view-only-nodes'
import type { JsonObject } from '@/electron/service-bridge/json-rpc-subprocess'
import type { NodeItem } from '@/shared/types/nodes'

let counter = 0

/** A fresh UUID per test: the bridge state is one singleton for the whole file. */
function freshUuid(): string {
    counter += 1
    return `00000000-0000-4000-8000-${String(counter).padStart(12, '0')}`
}

function viewOnlyEntry(
    nodeUuid: string,
    overrides: Partial<ViewOnlyNodeEntry> = {}
): ViewOnlyNodeEntry {
    return { name: 'lab-box', address: '192.0.2.50', port: 14318, nodeUuid, ...overrides }
}

function brokerSnapshot(nodes: { hostUuid: string; name: string; ipAddress: string }[]): void {
    getModularBridgeState().handleNotification({
        source: 'broker',
        method: 'discovery:nodes-changed',
        params: {
            nodes: nodes.map(node => ({ ...node, port: 14318, trusted: false, clustered: false }))
        }
    })
}

function nodeInfo(hostUuid: string, gpuCount = 1): JsonObject {
    return {
        hostUuid,
        GPUs: Array.from({ length: gpuCount }, (_unused, index) => ({
            name: `NVIDIA Test ${index}`,
            vram_bytes: 8_000_000_000,
            vram_used_bytes: 1_000_000_000,
            utilization_percent: 12
        })),
        cpu: { name: 'Test CPU', cores: 8, utilization_percent: 30 },
        memory: { total_bytes: 32_000_000_000, used_bytes: 8_000_000_000 }
    }
}

function shownNode(nodeUuid: string): NodeItem | undefined {
    return getModularBridgeState().getNodesInitial().nodes[nodeUuid]
}

describe('view-only nodes in the bridge state', () => {
    const channels: string[] = []
    const upserts: NodeItem[] = []
    const removals: string[] = []
    let unsubscribe = (): void => {}

    beforeEach(() => {
        mocks.warnings.length = 0
        channels.length = 0
        upserts.length = 0
        removals.length = 0
        unsubscribe = subscribePush(event => {
            channels.push(event.channel)
            if (event.channel === 'nodes:upsert') upserts.push(event.payload)
            if (event.channel === 'nodes:remove') removals.push(event.payload)
        })
    })

    afterEach(() => {
        unsubscribe()
        getModularBridgeState().setViewOnlyNodes([])
    })

    it('shows a configured node flagged view-only', () => {
        const nodeUuid = freshUuid()

        getModularBridgeState().setViewOnlyNodes([viewOnlyEntry(nodeUuid)])

        expect(shownNode(nodeUuid)).toMatchObject({
            id: nodeUuid,
            name: 'lab-box',
            status: 'active',
            ipAddress: '192.0.2.50',
            viewOnly: true
        })
    })

    it('never flags a discovered node view-only', () => {
        const nodeUuid = freshUuid()

        brokerSnapshot([{ hostUuid: nodeUuid, name: 'peer', ipAddress: '192.0.2.60' }])

        expect(shownNode(nodeUuid)?.viewOnly).toBeUndefined()
    })

    it('polls a configured node at its one configured address and port', () => {
        const nodeUuid = freshUuid()

        getModularBridgeState().setViewOnlyNodes([viewOnlyEntry(nodeUuid, { port: 14400 })])

        expect(
            getModularBridgeState()
                .getNodeInfoPollTargets()
                .filter(target => target.id === nodeUuid)
        ).toEqual([{ id: nodeUuid, hosts: ['192.0.2.50'], port: 14400, viewOnly: true }])
    })

    it('does not list a configured node among the available nodes', () => {
        const nodeUuid = freshUuid()

        getModularBridgeState().setViewOnlyNodes([viewOnlyEntry(nodeUuid)])

        expect(
            getModularBridgeState()
                .getAvailableNodes()
                .map(node => node.id)
        ).not.toContain(nodeUuid)
    })

    it('resolves no address or hostname for a configured node', () => {
        const nodeUuid = freshUuid()

        getModularBridgeState().setViewOnlyNodes([viewOnlyEntry(nodeUuid)])

        expect(getModularBridgeState().getNodeAddresses(nodeUuid)).toEqual([])
        expect(getModularBridgeState().getNodeHostname(nodeUuid)).toBe('')
    })

    it('reports a configured node as view-only to the handlers that guard on it', () => {
        const nodeUuid = freshUuid()

        getModularBridgeState().setViewOnlyNodes([viewOnlyEntry(nodeUuid)])

        expect(getModularBridgeState().isViewOnlyNode(nodeUuid)).toBe(true)
        expect(getModularBridgeState().isViewOnlyNode(freshUuid())).toBe(false)
    })

    it('keeps a configured node through a broker snapshot that does not list it', () => {
        const nodeUuid = freshUuid()
        getModularBridgeState().setViewOnlyNodes([viewOnlyEntry(nodeUuid)])

        brokerSnapshot([{ hostUuid: freshUuid(), name: 'peer', ipAddress: '192.0.2.61' }])
        brokerSnapshot([])

        expect(shownNode(nodeUuid)?.viewOnly).toBe(true)
    })

    it('keeps a configured node polled through a broker snapshot that does not list it', () => {
        const nodeUuid = freshUuid()
        getModularBridgeState().setViewOnlyNodes([viewOnlyEntry(nodeUuid)])

        brokerSnapshot([])

        expect(
            getModularBridgeState()
                .getNodeInfoPollTargets()
                .map(target => target.id)
        ).toContain(nodeUuid)
    })

    it('does not tell the renderer to remove a configured node on a broker snapshot', () => {
        const nodeUuid = freshUuid()
        getModularBridgeState().setViewOnlyNodes([viewOnlyEntry(nodeUuid)])
        removals.length = 0

        brokerSnapshot([])

        expect(removals).not.toContain(nodeUuid)
    })

    it('drops a configured node when a discovered node reports its UUID', () => {
        const nodeUuid = freshUuid()
        getModularBridgeState().setViewOnlyNodes([viewOnlyEntry(nodeUuid)])

        brokerSnapshot([{ hostUuid: nodeUuid, name: 'real-peer', ipAddress: '192.0.2.62' }])

        expect(shownNode(nodeUuid)).toMatchObject({ name: 'real-peer', ipAddress: '192.0.2.62' })
        expect(shownNode(nodeUuid)?.viewOnly).toBeUndefined()
    })

    it('stops polling a configured node once a discovered node reports its UUID', () => {
        const nodeUuid = freshUuid()
        getModularBridgeState().setViewOnlyNodes([viewOnlyEntry(nodeUuid)])

        brokerSnapshot([{ hostUuid: nodeUuid, name: 'real-peer', ipAddress: '192.0.2.62' }])

        expect(getModularBridgeState().isViewOnlyNode(nodeUuid)).toBe(false)
        expect(
            getModularBridgeState()
                .getNodeInfoPollTargets()
                .filter(target => target.id === nodeUuid)
                .map(target => target.viewOnly)
        ).toEqual([undefined])
    })

    it('logs when a discovered node displaces a configured one', () => {
        const nodeUuid = freshUuid()
        getModularBridgeState().setViewOnlyNodes([viewOnlyEntry(nodeUuid)])

        brokerSnapshot([{ hostUuid: nodeUuid, name: 'real-peer', ipAddress: '192.0.2.62' }])

        expect(mocks.warnings.some(message => message.includes('View-only node dropped'))).toBe(
            true
        )
    })

    it('does not add a configured node whose UUID is already discovered', () => {
        const nodeUuid = freshUuid()
        brokerSnapshot([{ hostUuid: nodeUuid, name: 'real-peer', ipAddress: '192.0.2.63' }])

        getModularBridgeState().setViewOnlyNodes([viewOnlyEntry(nodeUuid)])

        expect(getModularBridgeState().isViewOnlyNode(nodeUuid)).toBe(false)
        expect(shownNode(nodeUuid)?.viewOnly).toBeUndefined()
    })

    it('logs a configured node skipped for a discovered UUID', () => {
        const nodeUuid = freshUuid()
        brokerSnapshot([{ hostUuid: nodeUuid, name: 'real-peer', ipAddress: '192.0.2.63' }])

        getModularBridgeState().setViewOnlyNodes([viewOnlyEntry(nodeUuid)])

        expect(mocks.warnings.some(message => message.includes('View-only node skipped'))).toBe(
            true
        )
    })

    it('does not add a configured node that has this machine UUID', () => {
        const nodeUuid = freshUuid()
        getModularBridgeState().setSelfId(nodeUuid)

        getModularBridgeState().setViewOnlyNodes([viewOnlyEntry(nodeUuid)])

        expect(getModularBridgeState().isViewOnlyNode(nodeUuid)).toBe(false)
    })

    it('tells the renderer to remove a node that is no longer configured', () => {
        const nodeUuid = freshUuid()
        getModularBridgeState().setViewOnlyNodes([viewOnlyEntry(nodeUuid)])

        getModularBridgeState().setViewOnlyNodes([])

        expect(removals).toContain(nodeUuid)
    })

    it('shows the telemetry a matching answer reports', () => {
        const nodeUuid = freshUuid()
        getModularBridgeState().setViewOnlyNodes([viewOnlyEntry(nodeUuid)])

        getModularBridgeState().mergeNodeInfoResponse(nodeUuid, nodeInfo(nodeUuid))

        expect(shownNode(nodeUuid)?.topology.cpu.model).toBe('Test CPU')
        expect(shownNode(nodeUuid)?.topology.gpus).toHaveLength(1)
    })

    it('sends the renderer only the node and its metrics', () => {
        const nodeUuid = freshUuid()
        getModularBridgeState().setViewOnlyNodes([viewOnlyEntry(nodeUuid)])
        channels.length = 0

        getModularBridgeState().mergeNodeInfoResponse(nodeUuid, nodeInfo(nodeUuid))

        expect(new Set(channels)).toEqual(new Set(['nodes:upsert', 'metrics:update']))
    })

    it('sends no discovery or engine push when a node is configured', () => {
        channels.length = 0

        getModularBridgeState().setViewOnlyNodes([viewOnlyEntry(freshUuid())])

        expect(channels).not.toContain('discovery:nodes-changed')
        expect(channels).not.toContain('engines:state-changed')
    })

    it('shows no telemetry from an answer that names another host', () => {
        const nodeUuid = freshUuid()
        getModularBridgeState().setViewOnlyNodes([viewOnlyEntry(nodeUuid)])

        getModularBridgeState().mergeNodeInfoResponse(nodeUuid, nodeInfo(freshUuid()))

        expect(shownNode(nodeUuid)?.topology.cpu.model).toBe('')
        expect(shownNode(nodeUuid)?.topology.gpus).toEqual([])
    })

    it('shows no telemetry from an answer that names no host', () => {
        const nodeUuid = freshUuid()
        getModularBridgeState().setViewOnlyNodes([viewOnlyEntry(nodeUuid)])
        const unnamed = Object.fromEntries(
            Object.entries(nodeInfo(nodeUuid)).filter(([key]) => key !== 'hostUuid')
        )

        getModularBridgeState().mergeNodeInfoResponse(nodeUuid, unnamed)

        expect(shownNode(nodeUuid)?.topology.cpu.model).toBe('')
    })

    it('drops what an earlier matching answer showed when another host answers', () => {
        const nodeUuid = freshUuid()
        getModularBridgeState().setViewOnlyNodes([viewOnlyEntry(nodeUuid)])
        getModularBridgeState().mergeNodeInfoResponse(nodeUuid, nodeInfo(nodeUuid))

        getModularBridgeState().mergeNodeInfoResponse(nodeUuid, nodeInfo(freshUuid()))

        expect(shownNode(nodeUuid)?.topology.cpu.model).toBe('')
        expect(shownNode(nodeUuid)?.status).toBe('offline')
    })

    it('shows the telemetry again once the configured host answers', () => {
        const nodeUuid = freshUuid()
        getModularBridgeState().setViewOnlyNodes([viewOnlyEntry(nodeUuid)])
        getModularBridgeState().mergeNodeInfoResponse(nodeUuid, nodeInfo(freshUuid()))

        getModularBridgeState().mergeNodeInfoResponse(nodeUuid, nodeInfo(nodeUuid))

        expect(shownNode(nodeUuid)?.topology.cpu.model).toBe('Test CPU')
        expect(shownNode(nodeUuid)?.status).toBe('active')
    })

    it('shows at most the GPU rows a view-only node is allowed', () => {
        const nodeUuid = freshUuid()
        getModularBridgeState().setViewOnlyNodes([viewOnlyEntry(nodeUuid)])

        getModularBridgeState().mergeNodeInfoResponse(
            nodeUuid,
            nodeInfo(nodeUuid, VIEW_ONLY_MAX_GPUS + 30)
        )

        expect(shownNode(nodeUuid)?.topology.gpus).toHaveLength(VIEW_ONLY_MAX_GPUS)
    })
})

// The poller and the state together, with only the network stubbed: the view-only
// hostUuid rule is enforced by both, and this is the path a real answer takes.
describe('view-only nodes polled end to end', () => {
    afterEach(() => {
        stopNodeInfoPoller()
        vi.unstubAllGlobals()
        getModularBridgeState().setViewOnlyNodes([])
    })

    function answerWith(body: JsonObject): void {
        vi.stubGlobal(
            'fetch',
            vi.fn<typeof fetch>(() => Promise.resolve(new Response(JSON.stringify(body))))
        )
    }

    it('shows the telemetry of the configured host', async () => {
        const nodeUuid = freshUuid()
        getModularBridgeState().setViewOnlyNodes([viewOnlyEntry(nodeUuid)])
        answerWith(nodeInfo(nodeUuid))

        startNodeInfoPoller()

        await vi.waitFor(() => expect(shownNode(nodeUuid)?.topology.cpu.model).toBe('Test CPU'))
    })

    it('shows no telemetry when another host answers at the configured address', async () => {
        const nodeUuid = freshUuid()
        getModularBridgeState().setViewOnlyNodes([viewOnlyEntry(nodeUuid)])
        const fetchMock = vi.fn<typeof fetch>(() =>
            Promise.resolve(new Response(JSON.stringify(nodeInfo(freshUuid()))))
        )
        vi.stubGlobal('fetch', fetchMock)

        startNodeInfoPoller()

        await vi.waitFor(() => expect(fetchMock).toHaveBeenCalled())
        await vi.waitFor(() => expect(shownNode(nodeUuid)?.status).toBe('offline'))
        expect(shownNode(nodeUuid)?.topology.cpu.model).toBe('')
    })
})
