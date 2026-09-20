// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package main

// The CPU-identity repair is Linux-only: it reads the device tree, which no
// other platform exposes, and on Windows and macOS ghw already reports a
// complete model name and core count. These no-ops keep cpu_detect.go
// platform-neutral.

// cpuInfoOrFallback hands back exactly what ghw reported.
func cpuInfoOrFallback(name string, cores uint32) *CPUInfo {
	return &CPUInfo{Name: name, Cores: cores}
}

// cpuNameFallback has no non-Linux source of a CPU identity.
func cpuNameFallback() (name string, cores uint32) { return "", 0 }
