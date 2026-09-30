// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { AvailableNode, ClusterNode } from '@/shared/types/cluster'
import type { NodeItem } from '@/shared/types/nodes'

/**
 * The discovered peers an Invite button may target: not a cluster member, not
 * this machine, and never a view-only node, which is an untrusted machine shown
 * for its telemetry and must not be offered for pairing under any circumstance.
 *
 * Correlates on the node UUID: `AvailableNode.id` = `ClusterNode.nodeUuid` =
 * `selfId` = `NodeItem.id`. The hostname (`ClusterNode.id`) is display only and
 * must never be used to match a discovered node against a member or self.
 */
export function filterInvitablePeers(
    discovered: readonly AvailableNode[],
    members: readonly ClusterNode[],
    selfId: string | null,
    nodesMap: ReadonlyMap<string, NodeItem>
): AvailableNode[] {
    const memberUuids = new Set(members.map(member => member.nodeUuid))
    return discovered.filter(
        node =>
            !memberUuids.has(node.id) &&
            node.id !== selfId &&
            nodesMap.get(node.id)?.viewOnly !== true
    )
}
