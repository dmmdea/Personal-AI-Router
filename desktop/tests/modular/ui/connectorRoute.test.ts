// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import { directPath, roundedPath, routeToNode, type Rect } from '@/ui/components/Workloads/connectorRoute'

// A 3 x 2 grid like the operator's 4K window: 1100 px cards, 12 px gaps,
// starting 60 px right of the job list's edge (x = 320).
const W = 1100
const H = 500
const GAP = 12
const LEFT = 380
const TOP = 60
function card(col: number, row: number): Rect {
    const left = LEFT + col * (W + GAP)
    const top = TOP + row * (H + GAP)
    return { left, right: left + W, top, bottom: top + H }
}
const grid = [card(0, 0), card(1, 0), card(2, 0), card(0, 1), card(1, 1), card(2, 1)]
const start = { x: 320, y: 300 }

function numbers(d: string): number[] {
    return (d.match(/-?\d+(\.\d+)?/g) ?? []).map(Number)
}

// Every point of the path stays out of every card but the target (checked on
// the polyline vertices and on the segments between them).
function crossesACard(d: string, cards: readonly Rect[], target: Rect): boolean {
    const n = numbers(d)
    const pts: { x: number; y: number }[] = []
    for (let i = 0; i + 1 < n.length; i += 2) pts.push({ x: n[i], y: n[i + 1] })
    for (let i = 1; i < pts.length; i++) {
        for (let t = 0; t <= 20; t++) {
            const x = pts[i - 1].x + ((pts[i].x - pts[i - 1].x) * t) / 20
            const y = pts[i - 1].y + ((pts[i].y - pts[i - 1].y) * t) / 20
            for (const c of cards) {
                if (c === target) continue
                if (x > c.left + 1 && x < c.right - 1 && y > c.top + 1 && y < c.bottom - 1) return true
            }
        }
    }
    return false
}

describe('routeToNode', () => {
    it('keeps the direct line for a card that faces the job list', () => {
        const target = card(0, 1)
        expect(routeToNode(start, target, 800, grid)).toBe(directPath(start, { x: target.left, y: 800 }))
    })

    it('routes a line to column 3 through the gutters, never through a card', () => {
        const target = card(2, 0)
        const d = routeToNode(start, target, 300, grid)
        expect(d).not.toBe(directPath(start, { x: target.left, y: 300 }))
        expect(crossesACard(d, grid, target)).toBe(false)
        // Ends on the target's left edge at its anchor.
        expect(d.endsWith(`L ${target.left},300`)).toBe(true)
        // Travels along the gap between the rows.
        expect(numbers(d)).toContain(TOP + H + GAP / 2)
    })

    it('uses the gap above a card on the last row', () => {
        const target = card(1, 1)
        const d = routeToNode(start, target, 800, grid)
        expect(crossesACard(d, grid, target)).toBe(false)
        expect(numbers(d)).toContain(TOP + H + GAP / 2)
        expect(numbers(d)).toContain(card(0, 1).right + GAP / 2)
    })

    it('goes under a single row', () => {
        const row = [card(0, 0), card(1, 0), card(2, 0)]
        const target = row[1]
        const d = routeToNode(start, target, 300, row)
        expect(crossesACard(d, row, target)).toBe(false)
        expect(numbers(d)).toContain(TOP + H + 6)
    })
})

describe('roundedPath', () => {
    it('drops repeated points and rounds interior corners', () => {
        const d = roundedPath([
            { x: 0, y: 0 },
            { x: 0, y: 0 },
            { x: 100, y: 0 },
            { x: 100, y: 100 }
        ])
        expect(d.startsWith('M 0,0')).toBe(true)
        expect(d).toContain('Q 100,0')
        expect(d.endsWith('L 100,100')).toBe(true)
    })
})
