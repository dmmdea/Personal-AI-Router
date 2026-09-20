// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"context"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/jaypipes/ghw"
)

// nvidiaSmiTimeout caps how long we wait for a single nvidia-smi invocation.
// The tool is normally well under 100 ms, but on a wedged driver it can hang;
// a hard ceiling keeps both the one-shot startup detect and the per-tick stats
// collector from blocking indefinitely.
const nvidiaSmiTimeout = 3 * time.Second

// detectGPUs enumerates every GPU on Linux, from every vendor present, into
// one inventory.
//
// Each vendor has its own source, and each source is authoritative only for
// its own hardware: nvidia-smi for NVIDIA (marketing name, total VRAM, and a
// stable per-GPU UUID reused as the join key against the stats collector's
// snapshot), the Rockchip SoC detectors for a Mali GPU and an RKNPU (platform
// devices, not PCI adapters), amdgpu's sysfs nodes for Radeon, and i915/xe's
// sysfs nodes for Intel.
//
// The detectors are *composed*, not raced. Until this change the chain
// returned at the first source that produced anything, so a node with a
// discrete card and an integrated one published only the discrete card: an
// NVIDIA + Intel UHD 630 host listed one GPU and silently dropped the iGPU,
// and the same early return hid an AMD iGPU behind an NVIDIA card. A machine
// really does have both, and a node list that omits one is wrong about what
// the machine is.
//
// ghw stays the last resort and only when nothing else found anything at all:
// it reports adapter names with no VRAM and no join key, so those hosts list
// their GPUs without dynamic VRAM/utilization (the pre-existing non-Windows
// behavior). It must not run alongside the vendor detectors, because it would
// list the very same PCI adapters a second time under worse names.
//
// On unified-memory architectures (UMA, e.g. Grace-Blackwell / DGX Spark)
// nvidia-smi reports [N/A] for memory.total because the GPU shares system
// DRAM; in that case VramBytes is filled from detectMemoryTotal() instead.
func detectGPUs() []GPUInfo {
	return composeLinuxGPUs(
		detectNvidiaGPUs(),
		detectRockchipDevices(),
		detectAMDGPUs(drmClassDir),
		detectIntelGPUs(drmClassDir),
		detectGPUsGHW,
	)
}

// composeLinuxGPUs concatenates the per-vendor inventories in a fixed order —
// nvidia, rockchip, amd, intel — so a node's GPU list is stable across
// restarts and the discrete card a scheduler cares about stays first on the
// hosts that have one. ghw is called only when every vendor detector came back
// empty, which is why it is passed as a function rather than a slice.
func composeLinuxGPUs(nvidia, rockchip, amd, intel []GPUInfo, ghw func() []GPUInfo) []GPUInfo {
	gpus := make([]GPUInfo, 0, len(nvidia)+len(rockchip)+len(amd)+len(intel))
	gpus = append(gpus, nvidia...)
	gpus = append(gpus, rockchip...)
	gpus = append(gpus, amd...)
	gpus = append(gpus, intel...)
	if len(gpus) == 0 {
		return ghw()
	}
	return gpus
}

// detectNvidiaGPUs is the nvidia-smi half of the inventory, split out so the
// composition above reads as one list of peers. A missing binary (no NVIDIA
// driver) returns nil, which is not an error on an AMD/Intel-only host.
func detectNvidiaGPUs() []GPUInfo {
	out, err := nvidiaSmiCSV("uuid,name,memory.total")
	if err != nil {
		return nil
	}
	gpus, uma := parseNvidiaStatic(out)
	if len(gpus) == 0 || !uma {
		return gpus
	}
	total := detectMemoryTotal()
	if total == 0 {
		return gpus
	}
	for i := range gpus {
		if gpus[i].usesSystemMemoryUsage {
			gpus[i].VramBytes = total
		}
	}
	return gpus
}

// detectGPUsGHW is the ghw-based fallback, identical in spirit to the
// non-Windows/non-Linux path in gpu_other.go: enumerate display adapters and
// return names only (VramBytes stays 0, statsKey stays empty).
func detectGPUsGHW() []GPUInfo {
	gpu, err := ghw.GPU()
	if err != nil {
		log.Printf("GPU detection error: %v", err)
		return nil
	}
	var gpus []GPUInfo
	for _, card := range gpu.GraphicsCards {
		name := "Unknown"
		if card.DeviceInfo != nil && card.DeviceInfo.Product != nil {
			name = card.DeviceInfo.Product.Name
		}
		gpus = append(gpus, GPUInfo{Name: name})
	}
	return gpus
}

// nvidiaSmiCSV runs `nvidia-smi --query-gpu=<fields> --format=csv,noheader,nounits`
// and returns raw stdout. The caller parses the comma-separated rows. A missing
// binary (not on PATH) surfaces as an exec error, which callers treat as "no
// NVIDIA GPU data available" and degrade silently.
func nvidiaSmiCSV(fields string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), nvidiaSmiTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu="+fields,
		"--format=csv,noheader,nounits").Output()
	return string(out), err
}

// isNvidiaSmiNA reports whether an nvidia-smi CSV field is a "not
// applicable" sentinel rather than a numeric value. UMA platforms such as
// DGX Spark return [N/A] or [Not Supported] for GPU memory queries.
func isNvidiaSmiNA(s string) bool {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "[]")
	switch strings.ToLower(s) {
	case "n/a", "not supported":
		return true
	default:
		return false
	}
}

// parseNvidiaStatic decodes the static query (uuid,name,memory.total) into
// GPUInfo records. memory.total is reported in MiB (because of -nounits); we
// convert to bytes. Rows missing a uuid or name, or with an unparseable VRAM
// figure, are skipped rather than emitted with placeholder values. Each
// unified-memory row is marked so response assembly can source its used bytes
// from system memory without depending on dynamic nvidia-smi collection. The
// second return value reports whether any row was unified, allowing the caller
// to fetch the shared system-memory total only when needed.
func parseNvidiaStatic(out string) ([]GPUInfo, bool) {
	var gpus []GPUInfo
	var unifiedMemory bool
	for _, line := range strings.Split(out, "\n") {
		fields := splitCSVRow(line)
		if len(fields) < 3 {
			continue
		}
		uuid, name := fields[0], fields[1]
		if uuid == "" || name == "" {
			continue
		}
		var vramBytes uint64
		usesUnifiedMemory := isNvidiaSmiNA(fields[2])
		if usesUnifiedMemory {
			unifiedMemory = true
		} else if mib, err := strconv.ParseUint(fields[2], 10, 64); err == nil {
			vramBytes = mib * 1024 * 1024
		}
		gpus = append(gpus, GPUInfo{
			Name:                  name,
			VramBytes:             vramBytes,
			statsKey:              uuid,
			usesSystemMemoryUsage: usesUnifiedMemory,
		})
	}
	return gpus, unifiedMemory
}
