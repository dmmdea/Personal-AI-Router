// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { NodeItem } from '@/shared/types/nodes'

interface NodeCardControls {
    /** Engine start/stop switches and install buttons on the card and tray row. */
    engineToggles: boolean
    /** The engine settings section, which also holds install, model and port actions. */
    engineSettings: boolean
    /** The expandable performance chart and the button that opens it. */
    performanceChart: boolean
}

/**
 * What a node's card may offer. A view-only node is an untrusted machine shown for
 * its telemetry, so its card offers no engine control and no settings: nothing that
 * could reach the cluster or a service on its behalf. The performance chart stays: it
 * only draws the telemetry already polled, which is the reason the card exists.
 */
export function nodeCardControls(node: Pick<NodeItem, 'viewOnly'>): NodeCardControls {
    const interactive = node.viewOnly !== true
    return {
        engineToggles: interactive,
        engineSettings: interactive,
        performanceChart: true
    }
}
