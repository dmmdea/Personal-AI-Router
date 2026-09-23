// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import "log/slog"

// detectMemoryTotal returns GlobalMemoryStatusEx's TotalPhys: the physical
// memory Windows manages, and the figure readMemoryUsed subtracts AvailPhys
// from. It replaces the installed-DIMM sum (Win32_PhysicalMemory.Capacity
// through ghw), which also counted hardware-reserved memory that can never
// appear as used, so a full machine could not read 100 %. See systemMemTotal
// in memory_detect.go for the base every platform now shares.
//
// Returns 0 when the call fails, which drops the "memory" object from the
// response.
func detectMemoryTotal() uint64 {
	ms, ok := readMemoryStatus()
	if !ok {
		slog.Warn("memory total unavailable: GlobalMemoryStatusEx failed")
		return 0
	}
	return ms.TotalPhys
}
