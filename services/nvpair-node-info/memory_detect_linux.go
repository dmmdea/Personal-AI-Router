// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"log/slog"
	"os"
	"strconv"
	"strings"
)

// detectMemoryTotal returns /proc/meminfo's MemTotal in bytes: the memory the
// kernel manages, and the base parseMeminfoUsed computes used memory against
// (MemTotal - MemAvailable). Publishing both from one file is what makes
// used/total a true fraction; see systemMemTotal in memory_detect.go for why
// the online-memory-block count this replaced was not.
//
// Returns 0 when the file cannot be read or carries no MemTotal line, which
// drops the "memory" object from the response.
func detectMemoryTotal() uint64 {
	return memoryTotalFrom(procMeminfoPath)
}

// memoryTotalFrom is detectMemoryTotal against any meminfo file, so a test can
// hand it a fixture.
func memoryTotalFrom(path string) uint64 {
	data, err := os.ReadFile(path)
	if err != nil {
		slog.Warn("memory total unavailable: read /proc/meminfo failed", "err", err)
		return 0
	}
	total, ok := parseMeminfoTotal(string(data))
	if !ok {
		slog.Warn("memory total unavailable: /proc/meminfo has no MemTotal line")
		return 0
	}
	return total
}

// parseMeminfoTotal returns the MemTotal line in bytes (/proc/meminfo values
// are kB). ok is false when the line is missing or unparseable.
func parseMeminfoTotal(s string) (uint64, bool) {
	for _, line := range strings.Split(s, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "MemTotal:" {
			continue
		}
		v, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil || v == 0 {
			return 0, false
		}
		return v * 1024, true
	}
	return 0, false
}
