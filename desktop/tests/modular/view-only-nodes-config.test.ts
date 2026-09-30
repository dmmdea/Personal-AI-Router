// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import fs from 'fs'
import path from 'path'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { initPlatform } from '@/electron/globals'
import { MODULAR_NODE_INFO_DEFAULT_PORT } from '@/shared/constants/modular-runtime'
import type { JsonValue } from '@/electron/service-bridge/json-rpc-subprocess'
import { createTmpUserData } from '../fixtures/tmpdir'

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

import {
    VIEW_ONLY_MAX_NODES,
    loadViewOnlyNodes,
    parseViewOnlyNodes
} from '@/electron/service-bridge/view-only-nodes'

const UUID = '0b6f4c1e-8a52-4d3b-9f10-2c7e5a9d3b64'
const OTHER_UUID = '7d2a9e40-1c3f-4b8a-a5d6-90e1f2b3c4d5'

function validEntry(overrides: Record<string, JsonValue> = {}): JsonValue {
    return { name: 'Lab GPU box', address: '192.0.2.10', port: 14400, nodeUuid: UUID, ...overrides }
}

function entriesFor(value: JsonValue): string[] {
    return parseViewOnlyNodes(value).entries.map(entry => entry.nodeUuid)
}

describe('view-only nodes config parsing', () => {
    it('accepts a valid entry as written', () => {
        expect(parseViewOnlyNodes([validEntry()]).entries).toEqual([
            { name: 'Lab GPU box', address: '192.0.2.10', port: 14400, nodeUuid: UUID }
        ])
    })

    it('defaults the port to the node-info port', () => {
        const { entries } = parseViewOnlyNodes([
            { name: 'Lab', address: '192.0.2.10', nodeUuid: UUID }
        ])
        expect(entries[0].port).toBe(MODULAR_NODE_INFO_DEFAULT_PORT)
    })

    it('accepts an IPv6 literal', () => {
        expect(entriesFor([validEntry({ address: '2001:db8::10' })])).toEqual([UUID])
    })

    it('lowercases the node UUID so it can be compared with the reported hostUuid', () => {
        const { entries } = parseViewOnlyNodes([validEntry({ nodeUuid: UUID.toUpperCase() })])
        expect(entries[0].nodeUuid).toBe(UUID)
    })

    it('rejects a hostname address', () => {
        expect(entriesFor([validEntry({ address: 'gpu-box.local' })])).toEqual([])
    })

    it('rejects a localhost address', () => {
        expect(entriesFor([validEntry({ address: 'localhost' })])).toEqual([])
    })

    it('rejects a bracketed IPv6 address', () => {
        expect(entriesFor([validEntry({ address: '[2001:db8::10]' })])).toEqual([])
    })

    it('rejects an IPv6 address with a zone', () => {
        expect(entriesFor([validEntry({ address: 'fe80::1%eth0' })])).toEqual([])
    })

    it('rejects an address with a scheme or a port', () => {
        expect(entriesFor([validEntry({ address: 'http://192.0.2.10' })])).toEqual([])
        expect(entriesFor([validEntry({ address: '192.0.2.10:14318' })])).toEqual([])
    })

    it('rejects a port of zero', () => {
        expect(entriesFor([validEntry({ port: 0 })])).toEqual([])
    })

    it('rejects a port above 65535', () => {
        expect(entriesFor([validEntry({ port: 65536 })])).toEqual([])
    })

    it('rejects a fractional port', () => {
        expect(entriesFor([validEntry({ port: 14318.5 })])).toEqual([])
    })

    it('rejects a port given as a string', () => {
        expect(entriesFor([validEntry({ port: '14318' })])).toEqual([])
    })

    it('rejects an empty name', () => {
        expect(entriesFor([validEntry({ name: '' })])).toEqual([])
    })

    it('rejects a name of only spaces', () => {
        expect(entriesFor([validEntry({ name: '   ' })])).toEqual([])
    })

    it('rejects a name over 64 characters', () => {
        expect(entriesFor([validEntry({ name: 'n'.repeat(65) })])).toEqual([])
    })

    it('accepts a name of exactly 64 characters', () => {
        expect(entriesFor([validEntry({ name: 'n'.repeat(64) })])).toEqual([UUID])
    })

    it('rejects a name with a control character', () => {
        expect(entriesFor([validEntry({ name: 'lab\nbox' })])).toEqual([])
    })

    it('rejects a name with a bidi override', () => {
        expect(entriesFor([validEntry({ name: 'lab‮box' })])).toEqual([])
    })

    it('rejects a name that is not a string', () => {
        expect(entriesFor([validEntry({ name: 7 })])).toEqual([])
    })

    it('rejects a node UUID that is not canonical', () => {
        expect(entriesFor([validEntry({ nodeUuid: 'not-a-uuid' })])).toEqual([])
        expect(entriesFor([validEntry({ nodeUuid: UUID.replaceAll('-', '') })])).toEqual([])
    })

    it('rejects an entry with no node UUID', () => {
        expect(entriesFor([{ name: 'Lab', address: '192.0.2.10' }])).toEqual([])
    })

    it('rejects an entry that is not an object', () => {
        expect(entriesFor(['192.0.2.10', null, 4, [validEntry()]])).toEqual([])
    })

    it('skips only the invalid entry and keeps its neighbours', () => {
        const parsed = parseViewOnlyNodes([
            validEntry(),
            validEntry({
                address: 'gpu-box.local',
                nodeUuid: '11111111-1111-4111-8111-111111111111'
            }),
            validEntry({ nodeUuid: OTHER_UUID })
        ])
        expect(parsed.entries.map(entry => entry.nodeUuid)).toEqual([UUID, OTHER_UUID])
    })

    it('reports one reason per skipped entry', () => {
        const { skipped } = parseViewOnlyNodes([
            validEntry({ address: 'gpu-box.local' }),
            validEntry({ port: 0 })
        ])
        expect(skipped).toHaveLength(2)
    })

    it('does not put a skipped value in the reason', () => {
        const { skipped } = parseViewOnlyNodes([validEntry({ address: 'secret-host.internal' })])
        expect(skipped.join(' ')).not.toContain('secret-host')
    })

    it('skips a repeated node UUID after the first', () => {
        const { entries } = parseViewOnlyNodes([
            validEntry({ name: 'first' }),
            validEntry({ name: 'second', address: '192.0.2.11' })
        ])
        expect(entries.map(entry => entry.name)).toEqual(['first'])
    })

    it('uses at most 16 entries', () => {
        const many = Array.from({ length: VIEW_ONLY_MAX_NODES + 4 }, (_unused, index) =>
            validEntry({ nodeUuid: `00000000-0000-4000-8000-${String(index).padStart(12, '0')}` })
        )
        expect(parseViewOnlyNodes(many).entries).toHaveLength(VIEW_ONLY_MAX_NODES)
    })

    it('uses no entries from an object', () => {
        expect(parseViewOnlyNodes({ nodes: [validEntry()] }).entries).toEqual([])
    })

    it('uses no entries from a string', () => {
        expect(parseViewOnlyNodes('192.0.2.10').entries).toEqual([])
    })
})

describe('view-only nodes config file', () => {
    const userData = createTmpUserData()

    function pointAtUserData(): string {
        initPlatform({
            getUserData: () => userData.dir,
            getTemp: () => userData.dir,
            getResourcesPath: () => process.cwd(),
            getAppName: () => 'Personal AI Router'
        })
        return path.join(userData.dir, 'configs', 'view-only-nodes.json')
    }

    function writeConfig(contents: string): void {
        const filePath = pointAtUserData()
        fs.mkdirSync(path.dirname(filePath), { recursive: true })
        fs.writeFileSync(filePath, contents, 'utf8')
    }

    beforeEach(() => {
        mocks.warnings.length = 0
        pointAtUserData()
    })

    it('loads the entries of a valid file', () => {
        writeConfig(JSON.stringify([validEntry()]))
        expect(loadViewOnlyNodes().map(entry => entry.nodeUuid)).toEqual([UUID])
    })

    it('loads no nodes when the file is missing', () => {
        expect(loadViewOnlyNodes()).toEqual([])
    })

    it('says nothing when the file is missing', () => {
        loadViewOnlyNodes()
        expect(mocks.warnings).toEqual([])
    })

    it('loads no nodes from a file that is not JSON', () => {
        writeConfig('{ not json')
        expect(loadViewOnlyNodes()).toEqual([])
    })

    it('warns once about a file that is not JSON', () => {
        writeConfig('{ not json')
        loadViewOnlyNodes()
        expect(mocks.warnings).toHaveLength(1)
    })

    it('loads no nodes from a file that is not an array', () => {
        writeConfig(JSON.stringify({ name: 'Lab', address: '192.0.2.10', nodeUuid: UUID }))
        expect(loadViewOnlyNodes()).toEqual([])
    })

    it('warns once about a file that is not an array', () => {
        writeConfig(JSON.stringify({ name: 'Lab' }))
        loadViewOnlyNodes()
        expect(mocks.warnings).toHaveLength(1)
    })

    it('loads no nodes when the path is a directory', () => {
        fs.mkdirSync(pointAtUserData(), { recursive: true })
        expect(loadViewOnlyNodes()).toEqual([])
    })

    it('warns once per invalid entry', () => {
        writeConfig(
            JSON.stringify([validEntry(), validEntry({ port: 0 }), validEntry({ name: '' })])
        )
        loadViewOnlyNodes()
        expect(mocks.warnings).toHaveLength(2)
    })

    it('keeps the valid entries beside an invalid one', () => {
        writeConfig(JSON.stringify([validEntry({ port: 0 }), validEntry({ nodeUuid: OTHER_UUID })]))
        expect(loadViewOnlyNodes().map(entry => entry.nodeUuid)).toEqual([OTHER_UUID])
    })
})
