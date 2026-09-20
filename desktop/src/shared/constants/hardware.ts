// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * The value node-info puts in a GPU row's `memory_pool` when the device has no
 * memory of its own: an Intel or AMD integrated GPU, an Arm Mali GPU, an
 * RKNPU, an Apple Silicon GPU, an NVIDIA UMA part. Its `vram_bytes` is the
 * host's pool, shared with the CPU.
 *
 * It lives in `shared/` because both sides of the app compare against it: the
 * service bridge in the main process decides whether a row gets a VRAM series
 * at all, and the renderer decides how to label its memory line. A copy in
 * each would be two chances to drift from what the service sends.
 *
 * The literal matches `noderec.GPUMemoryPoolUnified` in the Go services, whose
 * own test pins it. Absent means the capacity is dedicated device memory.
 */
export const MEMORY_POOL_UNIFIED = 'unified'
