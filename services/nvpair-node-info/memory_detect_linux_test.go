// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// meminfoFixture is /proc/meminfo as a 64 GiB desktop reports it: MemTotal is
// what the kernel manages (62.66 GiB), well short of the installed 64 GiB and
// further still from the 66 GiB the online memory blocks add up to, because
// the 2 GiB blocks on each side of the PCI hole are only partly RAM.
const meminfoFixture = "MemTotal:       65705456 kB\n" +
	"MemFree:         9123456 kB\n" +
	"MemAvailable:   41869280 kB\n" +
	"Buffers:          812345 kB\n"

// TestMemoryTotalIsMeminfoMemTotal pins the Linux total to MemTotal, read from
// the same file the used figure is. The old source counted online sysfs memory
// blocks in full and published 70866960384 (33 x 2 GiB) on a box whose kernel
// manages 67282386944; the desktop then divided a MemTotal-based used figure
// by it and read RAM two points low.
func TestMemoryTotalIsMeminfoMemTotal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meminfo")
	if err := os.WriteFile(path, []byte(meminfoFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	const wantTotal uint64 = 65705456 * 1024
	total := memoryTotalFrom(path)
	if total != wantTotal {
		t.Fatalf("memoryTotalFrom = %d, want MemTotal %d", total, wantTotal)
	}
	const blockCountTotal uint64 = 33 << 31
	if total == blockCountTotal {
		t.Fatal("total is the online-memory-block count, not MemTotal")
	}

	// One base: used + MemAvailable == total, so used/total is a true fraction
	// and a fully used machine can read 100 %.
	used, ok := parseMeminfoUsed(meminfoFixture)
	if !ok {
		t.Fatal("parseMeminfoUsed failed on the fixture")
	}
	const available uint64 = 41869280 * 1024
	if used+available != total {
		t.Fatalf("used %d + available %d = %d, want the published total %d", used, available, used+available, total)
	}

	// And the pair reaches the wire unchanged.
	body := buildResponseAt(nil, nil, total, statsSnapshot{MemUsedBytes: used}, "", nil, time.Now())
	var resp struct {
		Memory struct {
			TotalBytes uint64 `json:"total_bytes"`
			UsedBytes  uint64 `json:"used_bytes"`
		} `json:"memory"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Memory.TotalBytes != total || resp.Memory.UsedBytes != used {
		t.Fatalf("memory = %+v, want total %d used %d", resp.Memory, total, used)
	}
}

func TestParseMeminfoTotal(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		want   uint64
		wantOK bool
	}{
		{name: "fixture", in: meminfoFixture, want: 65705456 * 1024, wantOK: true},
		{name: "MemTotal not first", in: "MemFree: 1 kB\nMemTotal:  2048 kB\n", want: 2048 * 1024, wantOK: true},
		{name: "missing", in: "MemFree: 1 kB\nMemAvailable: 2 kB\n"},
		{name: "unparseable", in: "MemTotal: lots kB\n"},
		{name: "zero", in: "MemTotal: 0 kB\n"},
		{name: "empty", in: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseMeminfoTotal(tc.in)
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("parseMeminfoTotal = (%d, %v), want (%d, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// TestMemoryTotalUnreadable drops the memory object rather than inventing a
// total.
func TestMemoryTotalUnreadable(t *testing.T) {
	if got := memoryTotalFrom(filepath.Join(t.TempDir(), "missing")); got != 0 {
		t.Fatalf("memoryTotalFrom(missing) = %d, want 0", got)
	}
}

// TestLiveMemoryTotalMatchesMeminfo checks the real host: the published total
// is /proc/meminfo's MemTotal, byte for byte.
func TestLiveMemoryTotalMatchesMeminfo(t *testing.T) {
	data, err := os.ReadFile(procMeminfoPath)
	if err != nil {
		t.Skipf("no %s on this host: %v", procMeminfoPath, err)
	}
	want, ok := parseMeminfoTotal(string(data))
	if !ok {
		t.Skip("this host's meminfo has no MemTotal line")
	}
	if got := detectMemoryTotal(); got != want {
		t.Fatalf("detectMemoryTotal = %d, want MemTotal %d (%s kB)", got, want, strconv.FormatUint(want/1024, 10))
	}
}
