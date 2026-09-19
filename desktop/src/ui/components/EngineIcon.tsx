// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { CSSProperties } from 'react'
import { type WorkloadEngine } from '@/shared/types/workloads'
import ollamaIcon from '@/ui/assets/engine-icons/ollama.png?inline'
import lmStudioIcon from '@/ui/assets/engine-icons/lm-studio.png?inline'

/**
 * Engine badge. Proxy engines get their logo; any other identifier (a
 * third-party producer on the workload-manager ingress, e.g. `llamacpp`,
 * `vllm`, `comfyui`) gets a generic monogram so the card still renders.
 */
export default function EngineIcon({ type, size = 32 }: { type: WorkloadEngine; size?: number }) {
    const dimension = `${size}px`
    const imgStyle: CSSProperties = { width: '100%', height: '100%', objectFit: 'contain' }
    const containerStyle: CSSProperties = {
        width: dimension,
        minWidth: dimension,
        maxWidth: dimension,
        height: dimension,
        minHeight: dimension,
        maxHeight: dimension,
        backgroundColor: '#fff',
        borderRadius: '25%',
        overflow: 'hidden'
    }

    if (type === 'ollama') {
        return (
            <div style={containerStyle}>
                <img src={ollamaIcon} alt="Ollama" style={imgStyle} />
            </div>
        )
    }

    if (type === 'lm-studio') {
        imgStyle.objectFit = 'cover'

        return (
            <div style={containerStyle}>
                <img src={lmStudioIcon} alt="LM Studio" style={imgStyle} />
            </div>
        )
    }

    // Unknown engine: a monogram of the identifier's first two letters on a
    // neutral tile, so a third-party producer's card carries a badge instead
    // of an empty slot.
    const label =
        type
            .replace(/[^a-z0-9]/gi, '')
            .slice(0, 2)
            .toUpperCase() || '?'
    const monogramStyle: CSSProperties = {
        ...containerStyle,
        backgroundColor: '#3a3a3a',
        color: '#e6e6e6',
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'center',
        fontSize: `${Math.max(8, Math.round(size * 0.42))}px`,
        fontWeight: 600,
        letterSpacing: '0.02em'
    }
    return (
        <div style={monogramStyle} title={type} aria-label={type}>
            {label}
        </div>
    )
}
