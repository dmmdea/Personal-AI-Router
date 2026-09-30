// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { NodeStatuses } from '@/shared/constants/nodes'
import { SystemTopology } from '@/shared/types/hardware'
import { PlatformDisplayName } from '@/shared/types/platform'

export type NodeStatus = (typeof NodeStatuses)[number]

export interface NodeItem {
    id: string
    name: string
    status: NodeStatus
    ipAddress: string
    port: number
    allIpAddresses: string[]
    topology: SystemTopology
    os: PlatformDisplayName
    /**
     * A machine configured for display only (`view-only-nodes.json`). It is not a
     * cluster member and is never trusted or paired: this app polls its node-info
     * endpoint and nothing else, so its card carries no engine controls or actions.
     */
    viewOnly?: boolean
}
