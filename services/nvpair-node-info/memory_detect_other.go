// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !darwin && !linux && !windows

package main

import (
	"log"

	"github.com/jaypipes/ghw"
)

// detectMemoryTotal returns total physical RAM in bytes via ghw on the
// platforms with no collector of their own (see stats_other.go). Those hosts
// publish no used figure at all, so there is no second base for this one to
// disagree with. Windows, Linux and macOS each read the total from the same
// source their collector reads used memory from — see memory_detect.go.
//
// Returns 0 on failure, which propagates through buildResponse as an absent
// "memory" object. ghw's TotalPhysicalBytes is an int64 (signed) for
// historical reasons; negatives are clamped to 0 defensively.
func detectMemoryTotal() uint64 {
	mem, err := ghw.Memory()
	if err != nil {
		log.Printf("memory detection error: %v", err)
		return 0
	}
	if mem == nil || mem.TotalPhysicalBytes <= 0 {
		return 0
	}
	return uint64(mem.TotalPhysicalBytes)
}
