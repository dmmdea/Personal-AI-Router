// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"nvpair-shared/noderec"
)

// The unified-memory contract, asserted where it is decided: response
// assembly. The detectors' own tests pin which rows get MemoryPool; this file
// pins what a client then receives, because the fault it exists for was not in
// a detector at all — every unified row was marked usesSystemMemoryUsage, and
// buildResponseAt dutifully copied the HOST's memory usage onto each of them.
//
// The result on the measured hosts: an idle Intel UHD Graphics 630 reported
// "25.5 GB / 66 GB" and an idle Mali-G610 reported "1.1 GB / 8 GB". Neither
// device had allocated any of it; both numbers were the box's own RAM usage
// wearing the GPU's label.
//
// The rule these tests hold in place: a shared capacity is marked as shared
// (memory_pool), and a used figure is published only by a row that MEASURES
// one. Sharing a pool is not a licence to invent what you are holding in it.

// sharedPoolUsed is the system-memory usage every case below is built against.
// It is deliberately unlike any per-device figure in these tests, so a row that
// leaks it is identified by value and not merely by being non-zero.
const sharedPoolUsed uint64 = 25_500_000_000

func TestBuildResponseUnifiedRowsPublishPoolNotBorrowedUsage(t *testing.T) {
	const (
		hostRAM    uint64 = 66 << 30
		apuPool    uint64 = 16 << 30
		apuUsed    uint64 = 3 << 30
		sparkPool  uint64 = 128 << 30
		discrete   uint64 = 16 << 30
		discUsed   uint64 = 2 << 30
		rockchipRA uint64 = 8 << 30
	)

	cases := []struct {
		name string
		row  GPUInfo
		stat *gpuStat
		// wantPool is the memory_pool value expected on the wire; "" means the
		// key must be absent entirely.
		wantPool string
		// wantUsed is the expected vram_used_bytes; 0 means the key must be
		// absent, which is how a client tells "nothing measured this" from a
		// device that measured zero.
		wantUsed uint64
		why      string
	}{
		{
			name:     "intel igpu",
			row:      GPUInfo{Name: "Intel UHD Graphics 630 (Coffee Lake, Gen 9.5)", VramBytes: hostRAM, statsKey: "intel:0000:00:02.0", MemoryPool: noderec.GPUMemoryPoolUnified},
			wantPool: noderec.GPUMemoryPoolUnified,
			why:      "i915 publishes no unprivileged per-device allocation counter",
		},
		{
			name:     "mali gpu",
			row:      GPUInfo{Name: "Arm Mali-G610 MP4", VramBytes: rockchipRA, statsKey: "mali:fb000000.gpu", MemoryPool: noderec.GPUMemoryPoolUnified},
			stat:     &gpuStat{UtilizationPct: 37, TemperatureC: 46},
			wantPool: noderec.GPUMemoryPoolUnified,
			why:      "the Mali driver reports load and temperature but no allocation",
		},
		{
			name:     "rknpu accelerator",
			row:      GPUInfo{Name: "Rockchip RK3588 NPU (3 cores)", Kind: noderec.GPUKindAccelerator, VramBytes: rockchipRA, statsKey: "rknpu:fdab0000.npu", MemoryPool: noderec.GPUMemoryPoolUnified},
			stat:     &gpuStat{UtilizationPct: 0, TemperatureC: 44},
			wantPool: noderec.GPUMemoryPoolUnified,
			why:      "the RKNPU driver reports load and temperature but no allocation",
		},
		{
			name:     "amd apu keeps its own measurement",
			row:      GPUInfo{Name: "AMD Radeon Graphics (Barcelo, GCN 5.1)", VramBytes: apuPool, statsKey: "amd:0000:04:00.0", MemoryPool: noderec.GPUMemoryPoolUnified},
			stat:     &gpuStat{VRAMUsed: apuUsed, UtilizationPct: 12},
			wantPool: noderec.GPUMemoryPoolUnified,
			wantUsed: apuUsed,
			why:      "amdgpu measures vram_used + gtt_used for this device specifically",
		},
		{
			name: "nvidia uma still maps system usage",
			row: GPUInfo{
				Name: "NVIDIA GB10", VramBytes: sparkPool, statsKey: "GPU-spark",
				MemoryPool: noderec.GPUMemoryPoolUnified, usesSystemMemoryUsage: true,
			},
			wantPool: noderec.GPUMemoryPoolUnified,
			wantUsed: sharedPoolUsed,
			why:      "one pool, one allocator: the host figure IS what the accelerator draws from",
		},
		{
			name:     "discrete card unchanged",
			row:      GPUInfo{Name: "NVIDIA GeForce RTX 5060 Ti", VramBytes: discrete, statsKey: "GPU-discrete"},
			stat:     &gpuStat{VRAMUsed: discUsed, UtilizationPct: 41},
			wantUsed: discUsed,
			why:      "a dedicated card's memory is its own and it reports its own usage",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			snap := statsSnapshot{MemUsedBytes: sharedPoolUsed}
			if c.stat != nil {
				snap.GPU = map[string]gpuStat{c.row.statsKey: *c.stat}
			}
			_, raw := buildResponseDecode(t, []GPUInfo{c.row}, nil, 0, snap)
			obj := soleGPUObject(t, raw)

			pool, present := obj["memory_pool"]
			switch {
			case c.wantPool == "" && present:
				t.Errorf("memory_pool = %v, want the key absent (%s)", pool, c.why)
			case c.wantPool != "" && pool != c.wantPool:
				t.Errorf("memory_pool = %v, want %q", pool, c.wantPool)
			}

			used, present := obj["vram_used_bytes"]
			if c.wantUsed == 0 {
				if present {
					t.Errorf("vram_used_bytes = %v, want the key absent: %s", used, c.why)
				}
				return
			}
			if !present {
				t.Fatalf("vram_used_bytes absent, want %d (%s)", c.wantUsed, c.why)
			}
			if got := uint64(used.(float64)); got != c.wantUsed {
				t.Errorf("vram_used_bytes = %d, want %d (%s)", got, c.wantUsed, c.why)
			}
		})
	}
}

// TestUnifiedRowKeepsEverythingItDoesMeasure is the other half of the rule.
// Dropping a fabricated number must not cost a row the readings it genuinely
// has: the Mali and RKNPU samplers publish load and temperature, and a fix
// that silenced those would trade one wrong figure for two missing ones.
func TestUnifiedRowKeepsEverythingItDoesMeasure(t *testing.T) {
	row := GPUInfo{
		Name: "Arm Mali-G610 MP4", VramBytes: 8 << 30,
		statsKey: "mali:fb000000.gpu", MemoryPool: noderec.GPUMemoryPoolUnified,
	}
	snap := statsSnapshot{
		MemUsedBytes: sharedPoolUsed,
		GPU:          map[string]gpuStat{"mali:fb000000.gpu": {UtilizationPct: 37, TemperatureC: 46}},
	}
	typed, _ := buildResponseDecode(t, []GPUInfo{row}, nil, 0, snap)
	got := typed.GPUs[0]
	if got.UtilizationPercent != 37 {
		t.Errorf("UtilizationPercent = %d, want 37", got.UtilizationPercent)
	}
	if got.TemperatureCelsius != 46 {
		t.Errorf("TemperatureCelsius = %d, want 46", got.TemperatureCelsius)
	}
	if got.VramBytes != 8<<30 {
		t.Errorf("VramBytes = %d, want the pool ceiling %d", got.VramBytes, uint64(8<<30))
	}
	if got.VramUsedBytes != 0 {
		t.Errorf("VramUsedBytes = %d, want 0: no source measures it", got.VramUsedBytes)
	}
}

// TestUnifiedRowNeverBorrowsHostUsageAcrossRows guards the loop rather than one
// row: a response carrying a shared-pool row beside an nvidia UMA row must put
// the host figure on exactly the one that asked for it. The old code had the
// flag on every unified row, so this mix is the shape that would regress.
func TestUnifiedRowNeverBorrowsHostUsageAcrossRows(t *testing.T) {
	rows := []GPUInfo{
		{Name: "NVIDIA GB10", VramBytes: 128 << 30, statsKey: "GPU-spark", MemoryPool: noderec.GPUMemoryPoolUnified, usesSystemMemoryUsage: true},
		{Name: "Intel UHD Graphics 630 (Coffee Lake, Gen 9.5)", VramBytes: 66 << 30, statsKey: "intel:0000:00:02.0", MemoryPool: noderec.GPUMemoryPoolUnified},
		{Name: "Rockchip RK3588 NPU (3 cores)", Kind: noderec.GPUKindAccelerator, VramBytes: 8 << 30, statsKey: "rknpu:fdab0000.npu", MemoryPool: noderec.GPUMemoryPoolUnified},
	}
	typed, _ := buildResponseDecode(t, rows, nil, 0, statsSnapshot{MemUsedBytes: sharedPoolUsed})
	if got := typed.GPUs[0].VramUsedBytes; got != sharedPoolUsed {
		t.Errorf("nvidia UMA VramUsedBytes = %d, want the system figure %d", got, sharedPoolUsed)
	}
	for _, gpu := range typed.GPUs[1:] {
		if gpu.VramUsedBytes != 0 {
			t.Errorf("%q: VramUsedBytes = %d, want 0 — the host figure leaked onto a row that measures nothing",
				gpu.Name, gpu.VramUsedBytes)
		}
		if gpu.MemoryPool != noderec.GPUMemoryPoolUnified {
			t.Errorf("%q: MemoryPool = %q, want %q", gpu.Name, gpu.MemoryPool, noderec.GPUMemoryPoolUnified)
		}
	}
}

// soleGPUObject returns the single decoded GPU row from a raw response body,
// so a case can assert on key PRESENCE — which a typed decode cannot see,
// because omitempty and a zero value are indistinguishable once unmarshaled.
func soleGPUObject(t *testing.T, raw map[string]any) map[string]any {
	t.Helper()
	gpus, ok := raw["GPUs"].([]any)
	if !ok || len(gpus) != 1 {
		t.Fatalf("unexpected GPUs payload: %v", raw["GPUs"])
	}
	obj, ok := gpus[0].(map[string]any)
	if !ok {
		t.Fatalf("GPU row is not an object: %v", gpus[0])
	}
	return obj
}
