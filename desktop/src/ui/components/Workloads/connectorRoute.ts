// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Connector routing for the job list -> node grid lines.
//
// A line used to be one cubic from the job card straight to the node card's
// left edge. With one column of node cards that edge always faces the job list,
// but the node grid lays cards out in up to three columns, and a line to a
// card in column two or three crossed BEHIND the opaque cards in front of it:
// only the stretch into the first card showed, so a job running on a node in
// column three read as a job on the node in column one. A line to a card with other cards in
// front of it now runs through the gutters instead, which are transparent:
// down the strip beside the job list, along the gap between two rows, and up
// the gap just left of its card.

export interface Rect {
    left: number
    right: number
    top: number
    bottom: number
}

export interface Point {
    x: number
    y: number
}

// Corner radius of a routed line, clamped per corner to what its segments allow.
const CORNER_RADIUS = 10

// The straight line the connector has always drawn.
export function directPath(start: Point, end: Point): string {
    const cpOff = Math.abs(end.x - start.x) * 0.5
    return `M ${start.x},${start.y} C ${start.x + cpOff},${start.y} ${end.x - cpOff},${end.y} ${end.x},${end.y}`
}

function overlapsVertically(a: Rect, b: Rect): boolean {
    return a.top < b.bottom && a.bottom > b.top
}

function overlapsHorizontally(a: Rect, b: Rect): boolean {
    return a.left < b.right && a.right > b.left
}

// An orthogonal polyline with rounded interior corners.
export function roundedPath(points: readonly Point[], radius = CORNER_RADIUS): string {
    const pts = points.filter(
        (p, i) => i === 0 || p.x !== points[i - 1].x || p.y !== points[i - 1].y
    )
    if (pts.length === 0) return ''
    let d = `M ${pts[0].x},${pts[0].y}`
    for (let i = 1; i < pts.length - 1; i++) {
        const prev = pts[i - 1]
        const cur = pts[i]
        const next = pts[i + 1]
        const inLen = Math.hypot(cur.x - prev.x, cur.y - prev.y)
        const outLen = Math.hypot(next.x - cur.x, next.y - cur.y)
        const r = Math.min(radius, inLen / 2, outLen / 2)
        const a = { x: cur.x - ((cur.x - prev.x) / inLen) * r, y: cur.y - ((cur.y - prev.y) / inLen) * r }
        const b = { x: cur.x + ((next.x - cur.x) / outLen) * r, y: cur.y + ((next.y - cur.y) / outLen) * r }
        d += ` L ${a.x},${a.y} Q ${cur.x},${cur.y} ${b.x},${b.y}`
    }
    const last = pts[pts.length - 1]
    return `${d} L ${last.x},${last.y}`
}

/**
 * The path from a job card (start, on its right edge) to a node card to its
 * right. `anchorY` is where the line enters the node card; `cards` is every
 * node card on screen (the target included). A card with no other card
 * between it and the job list keeps the direct line.
 */
export function routeToNode(
    start: Point,
    target: Rect,
    anchorY: number,
    cards: readonly Rect[]
): string {
    const end = { x: target.left, y: anchorY }
    const others = cards.filter(
        c => !(c.left === target.left && c.top === target.top && c.right === target.right)
    )
    // Cards on the target's row between the job list and the target.
    const inFront = others.filter(
        c => overlapsVertically(c, target) && c.right <= target.left + 1 && c.left >= start.x
    )
    if (inFront.length === 0) return directPath(start, end)

    // The vertical gutter just left of the target.
    const colGapX = (Math.max(...inFront.map(c => c.right)) + target.left) / 2

    // The horizontal gutter: below the target's row when a row follows,
    // otherwise above it, otherwise just under the grid.
    const sameColumn = others.filter(c => overlapsHorizontally(c, target))
    const below = sameColumn
        .filter(c => c.top >= target.bottom - 1)
        .sort((a, b) => a.top - b.top)[0]
    const above = sameColumn
        .filter(c => c.bottom <= target.top + 1)
        .sort((a, b) => b.bottom - a.bottom)[0]
    const gutterY = below
        ? (target.bottom + below.top) / 2
        : above
          ? (above.bottom + target.top) / 2
          : target.bottom + 6

    // The vertical run beside the job list: midway across the strip.
    const gridLeft = Math.min(...cards.map(c => c.left))
    const stripX = (start.x + gridLeft) / 2

    return roundedPath([
        start,
        { x: stripX, y: start.y },
        { x: stripX, y: gutterY },
        { x: colGapX, y: gutterY },
        { x: colGapX, y: anchorY },
        end
    ])
}
