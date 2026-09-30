// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { beforeEach, describe, expect, it, vi } from 'vitest'

const VIEW_ONLY = 'view-only-uuid'

const mocks = vi.hoisted(() => ({
    state: {
        getSelfId: vi.fn(() => 'local-node'),
        isViewOnlyNode: vi.fn((_nodeId: string) => false),
        getNodeAddresses: vi.fn((_nodeId: string): string[] => []),
        getNodeHostname: vi.fn((_nodeId: string) => '')
    },
    supervisor: {
        hasProcess: vi.fn(() => true),
        callProcess: vi.fn(() => Promise.resolve(null)),
        sendProcess: vi.fn(),
        reportError: vi.fn()
    }
}))

vi.mock('@/electron/service-bridge/modular-supervisor', () => ({
    getModularSupervisor: () => mocks.supervisor
}))
vi.mock('@/electron/service-bridge/modular-state', () => ({
    getModularBridgeState: () => mocks.state,
    isProxyEngine: () => false,
    isUpstreamUnreachableError: () => false,
    parseServiceErrors: () => []
}))
vi.mock('@/electron/service-bridge/manual-nodes-store', () => ({
    removeManualNodeEntry: vi.fn(),
    resolveManualNodeKey: () => null
}))
vi.mock('@/electron/model-hub', () => ({ getEngineHubModels: vi.fn() }))

import { handleServiceBridgeInvoke } from '@/electron/service-bridge/empty-handlers'

describe('actions aimed at a view-only node', () => {
    beforeEach(() => {
        vi.clearAllMocks()
        mocks.state.getSelfId.mockReturnValue('local-node')
        mocks.state.isViewOnlyNode.mockImplementation(nodeId => nodeId === VIEW_ONLY)
        mocks.supervisor.hasProcess.mockReturnValue(true)
    })

    it('refuses to remove it as a member', async () => {
        const result = await handleServiceBridgeInvoke('nodes:remove-member', {
            nodeId: VIEW_ONLY
        })

        expect(result).toEqual({ nodeId: VIEW_ONLY, removed: false })
    })

    it('sends the broker nothing when asked to remove it', async () => {
        await handleServiceBridgeInvoke('nodes:remove-member', { nodeId: VIEW_ONLY })

        expect(mocks.supervisor.callProcess).not.toHaveBeenCalled()
        expect(mocks.supervisor.sendProcess).not.toHaveBeenCalled()
    })

    it('reports that an engine command is not available on it', async () => {
        await handleServiceBridgeInvoke('engine:command', {
            command: 'toggle',
            engineType: 'ollama',
            nodeId: VIEW_ONLY
        })

        expect(mocks.supervisor.reportError).toHaveBeenCalledWith(
            'toggle is not available on a view-only node.',
            'warning',
            'engine-cmd:toggle'
        )
    })

    it('routes no engine command to it', async () => {
        await handleServiceBridgeInvoke('engine:command', {
            command: 'install',
            engineType: 'ollama',
            nodeId: VIEW_ONLY
        })

        expect(mocks.supervisor.callProcess).not.toHaveBeenCalled()
        expect(mocks.supervisor.sendProcess).not.toHaveBeenCalled()
    })

    it('still sends the broker a removal for an ordinary node', async () => {
        await handleServiceBridgeInvoke('nodes:remove-member', { nodeId: 'ordinary-node' })

        expect(mocks.supervisor.callProcess).toHaveBeenCalledWith('broker', 'node/remove', {
            id: 'ordinary-node'
        })
    })
})
