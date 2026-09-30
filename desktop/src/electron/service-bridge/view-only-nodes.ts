// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import fs from 'fs'
import { isIP } from 'net'
import path from 'path'
import { getPaths } from '@/electron/globals'
import { MODULAR_NODE_INFO_DEFAULT_PORT } from '@/shared/constants/modular-runtime'
import { createStructuredLogger } from '@/shared/utils/log'
import getErrorString from '@/shared/utils/get-error-string'
import type { JsonObject, JsonValue } from './json-rpc-subprocess'

const log = createStructuredLogger('service-bridge')

/** More entries than this are ignored: the file is a short hand-written list. */
export const VIEW_ONLY_MAX_NODES = 16

const NAME_MAX_LENGTH = 64
const UUID_PATTERN = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i
// Letters, marks, digits, punctuation, symbols and the plain space. That leaves
// out control and format characters (bidi overrides included), which is what
// would let a name impersonate another one on a card.
const NAME_PATTERN = /^[\p{L}\p{M}\p{N}\p{P}\p{S} ]+$/u

/**
 * A machine shown on the overview for its telemetry alone. It never joins the
 * cluster, is never trusted, and never reaches the broker, the proxies or the
 * scheduler; the only thing that talks to it is this app's node-info poll.
 */
export interface ViewOnlyNodeEntry {
    name: string
    /** An IPv4 or IPv6 literal, so no DNS answer can move the poll target. */
    address: string
    port: number
    /** The `hostUuid` the machine must report, lowercase. It is also the card's key. */
    nodeUuid: string
}

interface ViewOnlyNodeParse {
    entries: ViewOnlyNodeEntry[]
    /** One reason per skipped entry, naming its position and never its content. */
    skipped: string[]
}

function objectValue(value: JsonValue | undefined): JsonObject | null {
    if (!value || typeof value !== 'object' || Array.isArray(value)) return null
    return value
}

function validName(value: JsonValue | undefined): string | null {
    if (typeof value !== 'string') return null
    if (value.trim().length === 0 || !NAME_PATTERN.test(value)) return null
    return Array.from(value).length <= NAME_MAX_LENGTH ? value : null
}

// `isIP` alone would also take an IPv6 zone ("fe80::1%eth0"), which names a local
// interface rather than a machine.
function validAddress(value: JsonValue | undefined): string | null {
    if (typeof value !== 'string' || value.includes('%')) return null
    return isIP(value) === 0 ? null : value
}

function validPort(value: JsonValue | undefined): number | null {
    if (value === undefined) return MODULAR_NODE_INFO_DEFAULT_PORT
    if (typeof value !== 'number' || !Number.isInteger(value)) return null
    return value >= 1 && value <= 65535 ? value : null
}

function validUuid(value: JsonValue | undefined): string | null {
    return typeof value === 'string' && UUID_PATTERN.test(value) ? value.toLowerCase() : null
}

/** The first thing wrong with an entry, or the entry itself when nothing is. */
function parseEntry(value: JsonValue): ViewOnlyNodeEntry | string {
    const obj = objectValue(value)
    if (!obj) return 'not an object'
    const name = validName(obj.name)
    if (name === null) return `name must be 1-${NAME_MAX_LENGTH} printable characters`
    const address = validAddress(obj.address)
    if (address === null) return 'address must be an IPv4 or IPv6 literal (no hostname)'
    const port = validPort(obj.port)
    if (port === null) return 'port must be an integer from 1 to 65535'
    const nodeUuid = validUuid(obj.nodeUuid)
    if (nodeUuid === null) return 'nodeUuid must be a UUID'
    return { name, address, port, nodeUuid }
}

/**
 * Validate the parsed contents of `view-only-nodes.json`. Anything that is not
 * an array yields no nodes; an entry that fails validation is skipped alone.
 */
export function parseViewOnlyNodes(value: JsonValue): ViewOnlyNodeParse {
    if (!Array.isArray(value)) return { entries: [], skipped: ['the file is not a JSON array'] }

    const entries: ViewOnlyNodeEntry[] = []
    const skipped: string[] = []
    const seen = new Set<string>()
    if (value.length > VIEW_ONLY_MAX_NODES) {
        skipped.push(`only the first ${VIEW_ONLY_MAX_NODES} entries are used`)
    }
    for (const [index, item] of value.slice(0, VIEW_ONLY_MAX_NODES).entries()) {
        const entry = parseEntry(item)
        if (typeof entry === 'string') {
            skipped.push(`entry ${index}: ${entry}`)
        } else if (seen.has(entry.nodeUuid)) {
            skipped.push(`entry ${index}: nodeUuid repeats an earlier entry`)
        } else {
            seen.add(entry.nodeUuid)
            entries.push(entry)
        }
    }
    return { entries, skipped }
}

function configFilePath(): string {
    return path.join(getPaths().getUserData(), 'configs', 'view-only-nodes.json')
}

/**
 * Read `<userData>/configs/view-only-nodes.json`. A missing file means no
 * view-only nodes; an unreadable or malformed one means the same, with a warning,
 * and never stops startup.
 */
export function loadViewOnlyNodes(): ViewOnlyNodeEntry[] {
    let parsed: JsonValue
    try {
        const filePath = configFilePath()
        if (!fs.existsSync(filePath)) return []
        parsed = JSON.parse(fs.readFileSync(filePath, 'utf8'))
    } catch (err) {
        log.warn({
            sublevel: 'view-only-nodes',
            message: `View-only nodes ignored, the file could not be read: ${getErrorString(err)}`
        })
        return []
    }

    const { entries, skipped } = parseViewOnlyNodes(parsed)
    for (const reason of skipped) {
        log.warn({ sublevel: 'view-only-nodes', message: `View-only nodes: ${reason}` })
    }
    return entries
}
