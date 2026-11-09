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

interface Point {
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
        const a = {
            x: cur.x - ((cur.x - prev.x) / inLen) * r,
            y: cur.y - ((cur.y - prev.y) / inLen) * r
        }
        const b = {
            x: cur.x + ((next.x - cur.x) / outLen) * r,
            y: cur.y + ((next.y - cur.y) / outLen) * r
        }
        d += ` L ${a.x},${a.y} Q ${cur.x},${cur.y} ${b.x},${b.y}`
    }
    const last = pts[pts.length - 1]
    return `${d} L ${last.x},${last.y}`
}

// Spacing between two lanes, and the clearance a lane keeps from a gutter's edge.
const LANE_GAP = 4
const LANE_EDGE = 2

/**
 * Which of `count` routed lines this one is. Routed lines share the strip beside
 * the job list and the gutters between the cards, and until 2026-11-09 they ran
 * on identical pixels there: a queued (yellow) and a running (blue) line to one
 * node blended into a grey-green line that read as neither, and lines to
 * different nodes could not be told apart where they shared a gutter.
 */
export interface Lane {
    index: number
    count: number
}

/**
 * This lane's offset from the centre of a gutter `width` wide: the lanes are
 * spread LANE_GAP apart around the centre, packed closer when the gutter is too
 * narrow to hold them, and always LANE_EDGE clear of its edges.
 */
export function laneOffset(lane: Lane | undefined, width: number): number {
    if (!lane || lane.count <= 1) return 0
    const half = Math.max(0, width / 2 - LANE_EDGE)
    const spread = Math.min(half, ((lane.count - 1) * LANE_GAP) / 2)
    return ((lane.index / (lane.count - 1)) * 2 - 1) * spread
}

/**
 * The path from a job card (start, on its right edge) to a node card to its
 * right. `anchorY` is where the line enters the node card; `cards` is every
 * node card on screen (the target included). A card with no other card
 * between it and the job list keeps the direct line. `lane` separates this
 * line from the other routed lines (see {@link Lane}).
 */
export function routeToNode(
    start: Point,
    target: Rect,
    anchorY: number,
    cards: readonly Rect[],
    lane?: Lane
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
    const inFrontRight = Math.max(...inFront.map(c => c.right))
    const colGapX = (inFrontRight + target.left) / 2 + laneOffset(lane, target.left - inFrontRight)

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
        ? (target.bottom + below.top) / 2 + laneOffset(lane, below.top - target.bottom)
        : above
          ? (above.bottom + target.top) / 2 + laneOffset(lane, target.top - above.bottom)
          : target.bottom + 6 + laneOffset(lane, 12)

    // The vertical run beside the job list: midway across the strip.
    const gridLeft = Math.min(...cards.map(c => c.left))
    const stripX = (start.x + gridLeft) / 2 + laneOffset(lane, gridLeft - start.x)

    // Lines to one card enter it at slightly different heights, so their last
    // stretch does not coincide either.
    const entryY = anchorY + laneOffset(lane, 16)

    return roundedPath([
        start,
        { x: stripX, y: start.y },
        { x: stripX, y: gutterY },
        { x: colGapX, y: gutterY },
        { x: colGapX, y: entryY },
        { x: end.x, y: entryY }
    ])
}
