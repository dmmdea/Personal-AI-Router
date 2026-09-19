// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'

import { parseWorkloadsInitial } from '@/electron/service-bridge/modular-state'

// A workload the broker stored is a card the desktop shows. Proxy engines map
// onto the closed union (`lmstudio` -> `lm-studio`); an identifier from a
// third-party producer on the workload-manager's loopback ingress is kept
// verbatim instead of the whole card being dropped.
describe('workload engine passthrough', () => {
    it('keeps a third-party engine identifier and still maps the proxy engines', () => {
        const parsed = parseWorkloadsInitial({
            workloads: [
                { id: '1', engine: 'llamacpp', state: 'running', model: 'qwen3.5-9b-agent', originatedFrom: 'u1', createdAt: 1 },
                { id: '2', engine: 'vllm', state: 'completed', model: 'qwen38-27b', originatedFrom: 'u1', createdAt: 2 },
                { id: '3', engine: 'lmstudio', state: 'queued', model: 'm', originatedFrom: 'u1', createdAt: 3 },
                { id: '4', engine: 'ollama', state: 'queued', model: 'm', originatedFrom: 'u1', createdAt: 4 }
            ]
        })
        expect(parsed.map((w) => [w.id, w.engine])).toEqual([
            ['1', 'llamacpp'],
            ['2', 'vllm'],
            ['3', 'lm-studio'],
            ['4', 'ollama']
        ])
    })

    it('still drops a workload with no engine at all', () => {
        const parsed = parseWorkloadsInitial({
            workloads: [{ id: '1', state: 'running', model: 'm', originatedFrom: 'u1', createdAt: 1 }]
        })
        expect(parsed).toEqual([])
    })
})
