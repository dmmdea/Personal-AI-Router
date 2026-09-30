// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import http from 'http'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
    MODULAR_NODE_INFO_MAX_BODY_BYTES,
    MODULAR_NODE_INFO_POLL_TIMEOUT_MS
} from '@/shared/constants/modular-runtime'
import type { getModularBridgeState } from '@/electron/service-bridge/modular-state'

type BridgeState = ReturnType<typeof getModularBridgeState>

const mocks = vi.hoisted(() => ({
    state: {
        getNodeInfoPollTargets: vi.fn<BridgeState['getNodeInfoPollTargets']>(),
        mergeNodeInfoResponse: vi.fn<BridgeState['mergeNodeInfoResponse']>()
    },
    warnings: [] as { message: string; reason: string }[]
}))

vi.mock('@/electron/service-bridge/modular-state', () => ({
    getModularBridgeState: () => mocks.state
}))

vi.mock('@/shared/utils/log', () => ({
    createStructuredLogger: () => ({
        info: (): void => {},
        warn: (payload: { message: string; data?: { reason?: string } }): void => {
            mocks.warnings.push({ message: payload.message, reason: payload.data?.reason ?? '' })
        },
        error: (): void => {},
        verbose: (): void => {}
    })
}))

import { startNodeInfoPoller, stopNodeInfoPoller } from '@/electron/service-bridge/node-info-poller'

const NODE_ID = 'node-under-test'
const servers: http.Server[] = []

async function listen(handler: http.RequestListener): Promise<number> {
    const server = http.createServer(handler)
    servers.push(server)
    await new Promise<void>(resolve => server.listen(0, '127.0.0.1', resolve))
    const address = server.address()
    if (!address || typeof address === 'string') throw new Error('server has no port')
    return address.port
}

function pollNodeOn(port: number, viewOnly = false): void {
    mocks.state.getNodeInfoPollTargets.mockReturnValue([
        { id: NODE_ID, hosts: ['127.0.0.1'], port, ...(viewOnly ? { viewOnly } : {}) }
    ])
    startNodeInfoPoller()
}

/** A valid node-info answer padded to `bytes`, so only its size decides whether it is read. */
function bodyOfSize(bytes: number, hostUuid = NODE_ID): string {
    const frame = JSON.stringify({ hostUuid, pad: '' })
    return JSON.stringify({ hostUuid, pad: 'a'.repeat(bytes - frame.length) })
}

function outageReason(): string {
    return mocks.warnings.find(entry => entry.message.includes('not answering'))?.reason ?? ''
}

async function untilOutageReported(): Promise<void> {
    await vi.waitFor(() => expect(outageReason()).not.toBe(''), { timeout: 5_000 })
}

describe('node info poll hardening', () => {
    beforeEach(() => {
        mocks.state.getNodeInfoPollTargets.mockReturnValue([])
        mocks.warnings.length = 0
    })

    afterEach(async () => {
        stopNodeInfoPoller()
        for (const server of servers.splice(0)) {
            server.closeAllConnections()
            await new Promise<void>(resolve => server.close(() => resolve()))
        }
    })

    it('merges an ordinary answer', async () => {
        const port = await listen((_req, res) => {
            res.setHeader('content-type', 'application/json')
            res.end(bodyOfSize(2_000))
        })

        pollNodeOn(port)

        await vi.waitFor(() => expect(mocks.state.mergeNodeInfoResponse).toHaveBeenCalledTimes(1))
    })

    it('does not follow a redirect', async () => {
        const target = vi.fn()
        const targetPort = await listen((_req, res) => {
            target()
            res.end(bodyOfSize(2_000))
        })
        const port = await listen((_req, res) => {
            res.writeHead(302, { location: `http://127.0.0.1:${targetPort}/v1/node-info` })
            res.end()
        })

        pollNodeOn(port)
        await untilOutageReported()

        expect(target).not.toHaveBeenCalled()
    })

    it('merges nothing from a redirecting node', async () => {
        const targetPort = await listen((_req, res) => res.end(bodyOfSize(2_000)))
        const port = await listen((_req, res) => {
            res.writeHead(307, { location: `http://127.0.0.1:${targetPort}/v1/node-info` })
            res.end()
        })

        pollNodeOn(port)
        await untilOutageReported()

        expect(mocks.state.mergeNodeInfoResponse).not.toHaveBeenCalled()
    })

    it('accepts a body just under the cap', async () => {
        const port = await listen((_req, res) => {
            res.end(bodyOfSize(MODULAR_NODE_INFO_MAX_BODY_BYTES - 1))
        })

        pollNodeOn(port)

        await vi.waitFor(() => expect(mocks.state.mergeNodeInfoResponse).toHaveBeenCalledTimes(1))
    })

    it('refuses a body whose declared length is over the cap', async () => {
        const body = bodyOfSize(MODULAR_NODE_INFO_MAX_BODY_BYTES + 1)
        const port = await listen((_req, res) => {
            res.writeHead(200, { 'content-length': Buffer.byteLength(body) })
            res.end(body)
        })

        pollNodeOn(port)
        await untilOutageReported()

        expect(mocks.state.mergeNodeInfoResponse).not.toHaveBeenCalled()
    })

    it('names the size limit when the declared length is over the cap', async () => {
        const body = bodyOfSize(MODULAR_NODE_INFO_MAX_BODY_BYTES + 1)
        const port = await listen((_req, res) => {
            res.writeHead(200, { 'content-length': Buffer.byteLength(body) })
            res.end(body)
        })

        pollNodeOn(port)
        await untilOutageReported()

        expect(outageReason()).toContain(`over ${MODULAR_NODE_INFO_MAX_BODY_BYTES} bytes`)
    })

    it('refuses a streamed body that grows past the cap with no declared length', async () => {
        const body = bodyOfSize(MODULAR_NODE_INFO_MAX_BODY_BYTES * 2)
        const port = await listen((_req, res) => {
            // Chunked: no content-length, so only counting the stream can stop it.
            res.on('error', () => {})
            const chunkSize = 16 * 1024
            for (let offset = 0; offset < body.length; offset += chunkSize) {
                res.write(body.slice(offset, offset + chunkSize))
            }
            res.end()
        })

        pollNodeOn(port)
        await untilOutageReported()

        expect(mocks.state.mergeNodeInfoResponse).not.toHaveBeenCalled()
    })

    it('names the size limit when a streamed body grows past the cap', async () => {
        const body = bodyOfSize(MODULAR_NODE_INFO_MAX_BODY_BYTES * 2)
        const port = await listen((_req, res) => {
            res.on('error', () => {})
            res.write(body)
            res.end()
        })

        pollNodeOn(port)
        await untilOutageReported()

        expect(outageReason()).toContain(`over ${MODULAR_NODE_INFO_MAX_BODY_BYTES} bytes`)
    })

    it('cuts off a body that starts and never finishes', async () => {
        const port = await listen((_req, res) => {
            res.on('error', () => {})
            res.writeHead(200, { 'content-type': 'application/json' })
            res.write('{"hostUuid":')
        })

        pollNodeOn(port)
        await untilOutageReported()

        expect(outageReason()).toBe(`no response within ${MODULAR_NODE_INFO_POLL_TIMEOUT_MS}ms`)
    })

    it('treats a body that is not JSON as a failed poll', async () => {
        const port = await listen((_req, res) => {
            res.end('<html>not node-info</html>')
        })

        pollNodeOn(port)
        await untilOutageReported()

        expect(mocks.state.mergeNodeInfoResponse).not.toHaveBeenCalled()
    })

    it('accepts a view-only answer that names the configured node', async () => {
        const port = await listen((_req, res) => res.end(bodyOfSize(2_000)))

        pollNodeOn(port, true)

        await vi.waitFor(() => expect(mocks.state.mergeNodeInfoResponse).toHaveBeenCalledTimes(1))
    })

    it('refuses a view-only answer that names another host', async () => {
        const port = await listen((_req, res) => res.end(bodyOfSize(2_000, 'some-other-host')))

        pollNodeOn(port, true)
        await untilOutageReported()

        expect(outageReason()).toBe('hostUuid does not match the configured node')
    })

    it('refuses a view-only answer that names no host', async () => {
        const port = await listen((_req, res) => res.end(JSON.stringify({ cpu: { cores: 8 } })))

        pollNodeOn(port, true)
        await untilOutageReported()

        expect(outageReason()).toBe('hostUuid does not match the configured node')
    })

    it('does not put the host a view-only answer claimed into the log', async () => {
        const port = await listen((_req, res) => res.end(bodyOfSize(2_000, 'claimed-identity')))

        pollNodeOn(port, true)
        await untilOutageReported()

        expect(JSON.stringify(mocks.warnings)).not.toContain('claimed-identity')
    })

    it('still lets an ordinary node answer without naming itself', async () => {
        const port = await listen((_req, res) => res.end(JSON.stringify({ cpu: { cores: 8 } })))

        pollNodeOn(port)

        await vi.waitFor(() => expect(mocks.state.mergeNodeInfoResponse).toHaveBeenCalledTimes(1))
    })
})
