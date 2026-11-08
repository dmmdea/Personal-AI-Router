// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"nvpair-shared/noderec"
)

// Axera AX650N accelerator cards (the M.2 "AX8850" and its relatives) inventory
// and sampling.
//
// The card is a PCIe endpoint (vendor 0x1f4b, class "Multimedia video
// controller") driven by the vendor's axcl_host / ax_pcie_host_dev modules. It
// has its own SoC, its own 4 GB of DRAM and its own Linux, so the host kernel
// exposes no counters for it: the only supported readout is the vendor CLI
// axcl-smi. The row is listed in the same inventory as a Coral Edge TPU or an
// RKNPU, with Kind = noderec.GPUKindAccelerator: an inference accelerator PAIR's
// engines cannot run on, so it never counts toward GPU pressure and never marks
// GPU telemetry fresh.
//
// Inventory comes from PCI (every vendor 0x1f4b device), so the card is listed
// even when axcl-smi is missing or failing — then it carries a name and
// utilization_unavailable, and no memory or temperature.
//
// The dynamic numbers come from ONE axcl-smi table call every axeraSampleInterval
// (a call takes ~0.6 s and opens a session on the card, so it is not run on the
// collector's 1 s tick). The table is the single source because it is the one
// place every figure is truthful:
//
//   - utilization: the NPU column. Measured under a looping model run, it reads
//     0 % idle and 89-92 % under load. "axcl-smi info --npu" is NOT used: it
//     prints "NPU Usage (%) : --" at idle and a differently shaped
//     "vnpu-Non : 92%%" line under load, so it cannot tell idle from unreadable.
//     A "--" in the table's NPU column is treated as no reading, never as 0.
//   - memory: the CMM pool (Contiguous Memory Manager), which is what NPU models
//     and media buffers allocate from. The card's "Memory-Usage" column is its
//     own Linux RAM, not the NPU's, so it is not reported. The pool belongs to
//     the card, so the row is not memory_pool:"unified".
//   - temperature: the card temperature, whole degrees.
//   - power: the card reports none ("-- / --"), so power_watts is omitted.
//
// The sampler keeps its last good table for axeraFreshFor; once that is older the
// dynamic fields are dropped, so a wedged CLI never freezes a stale figure.

const (
	axeraPCIVendor     = "1f4b"
	axeraPCIDevicesDir = "/sys/bus/pci/devices"
	axeraStatsPrefix   = "axera:"

	axeraSMIPath        = "/usr/bin/axcl/axcl-smi"
	axeraSampleInterval = 5 * time.Second
	axeraSMITimeout     = 5 * time.Second
	// axeraFreshFor is how old a good sample may be and still be published:
	// four sample intervals, so a single slow or failed call does not blank the
	// row but a dead CLI does.
	axeraFreshFor = 20 * time.Second
)

// axeraProductNames maps a PCI "vendor:device" id pair to the display name used
// when axcl-smi gives none. Unknown ids get a generic label carrying the ids.
var axeraProductNames = map[string]string{
	"1f4b:0650": "Axera AX650N",
}

// axeraChips maps the card name axcl-smi prints to the SoC on it.
var axeraChips = map[string]string{
	"AX8850": "AX650N",
}

// axeraDevice is one Axera PCI function.
type axeraDevice struct {
	bdf    string // lower-case PCI address, e.g. 0000:04:00.0
	vendor string
	device string
}

func axeraStatsKey(bdf string) string { return axeraStatsPrefix + strings.ToLower(bdf) }

// listAxeraDevices scans the PCI bus for Axera functions, sorted by address.
func listAxeraDevices(dir string) []axeraDevice {
	var out []axeraDevice
	for _, name := range sortedDirNames(dir) {
		vendor := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(readSysfs(filepath.Join(dir, name, "vendor")))), "0x")
		if vendor != axeraPCIVendor {
			continue
		}
		device := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(readSysfs(filepath.Join(dir, name, "device")))), "0x")
		out = append(out, axeraDevice{bdf: strings.ToLower(name), vendor: vendor, device: device})
	}
	return out
}

// axeraPresenceName is the name of a card axcl-smi has not (yet) described.
func axeraPresenceName(vendor, device string) string {
	if name, ok := axeraProductNames[vendor+":"+device]; ok {
		return name
	}
	if device == "" {
		return "Axera accelerator"
	}
	return "Axera accelerator (" + vendor + ":" + device + ")"
}

// axeraCardName is "Axera AX8850 (AX650N)" from the name axcl-smi prints.
func axeraCardName(cardName string) string {
	cardName = strings.TrimSpace(cardName)
	if cardName == "" {
		return ""
	}
	name := "Axera " + cardName
	if chip, ok := axeraChips[cardName]; ok {
		name += " (" + chip + ")"
	}
	return name
}

// axeraRow is the inventory row. Capacity, usage, utilization and temperature
// are filled from the sampler by buildResponse (statsKey join); until the first
// good sample, and whenever the sample is stale, the row says
// utilization_unavailable. It is not inference-ready: no engine PAIR runs has an
// Axera backend.
func axeraRow(dev axeraDevice, name string) GPUInfo {
	if name == "" {
		name = axeraPresenceName(dev.vendor, dev.device)
	}
	return GPUInfo{
		Name:                   name,
		Kind:                   noderec.GPUKindAccelerator,
		InferenceReady:         notInferenceReady(),
		statsKey:               axeraStatsKey(dev.bdf),
		utilizationNeedsSample: true,
	}
}

// axclCard is one card block of the axcl-smi table.
type axclCard struct {
	index    int
	name     string
	busID    string // lower-case PCI address
	tempC    uint32
	tempOK   bool
	npuPct   uint32
	npuOK    bool // false for "--": no reading, not idle
	cmmUsed  uint64
	cmmTotal uint64
	cmmOK    bool
}

var (
	axclBusIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{4}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-7]$`)
	axclTempPattern  = regexp.MustCompile(`(\d+)\s*C\b`)
	axclUtilPattern  = regexp.MustCompile(`^(\d+|--)\s*%?\s+(\d+|--)\s*%?$`)
	axclMemPattern   = regexp.MustCompile(`^(\d+)\s*([KMG]iB)\s*/\s*(\d+)\s*([KMG]iB)$`)
)

func axclUnitBytes(unit string) uint64 {
	switch unit {
	case "KiB":
		return 1 << 10
	case "MiB":
		return 1 << 20
	case "GiB":
		return 1 << 30
	}
	return 0
}

// parseAxclSmiTable decodes the card blocks of the default axcl-smi table:
//
//	|    0  AX8850                    V3.16.0 | 0000:04:00.0 |    151 MiB /   945 MiB |
//	|   --   33C                      -- / -- | 0%        0% |     18 MiB /  2944 MiB |
//
// A card is two consecutive three-cell rows: the first has an index/name/firmware
// cell and a PCI address, the second the fan/temperature/power cell, the CPU and
// NPU percentages and the CMM usage. Rows with another cell count (the header,
// the process list) are skipped, so a card with processes on it parses the same.
// Fields that do not parse are left unset (…OK false) rather than zeroed.
func parseAxclSmiTable(s string) []axclCard {
	var cards []axclCard
	var cur *axclCard
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if len(line) < 2 || line[0] != '|' || line[len(line)-1] != '|' {
			cur = nil
			continue
		}
		cells := strings.Split(line[1:len(line)-1], "|")
		if len(cells) != 3 {
			cur = nil
			continue
		}
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		if axclBusIDPattern.MatchString(cells[1]) {
			f := strings.Fields(cells[0])
			if len(f) < 2 {
				cur = nil
				continue
			}
			idx, err := strconv.Atoi(f[0])
			if err != nil {
				cur = nil
				continue
			}
			rest := f[1:]
			if len(rest) > 1 {
				rest = rest[:len(rest)-1] // trailing firmware version
			}
			cards = append(cards, axclCard{
				index: idx,
				name:  strings.Join(rest, " "),
				busID: strings.ToLower(cells[1]),
			})
			cur = &cards[len(cards)-1]
			continue
		}
		if cur == nil {
			continue
		}
		// Second row of the block.
		if m := axclTempPattern.FindStringSubmatch(cells[0]); m != nil {
			if v, err := strconv.ParseUint(m[1], 10, 32); err == nil {
				cur.tempC, cur.tempOK = uint32(v), true
			}
		}
		if m := axclUtilPattern.FindStringSubmatch(cells[1]); m != nil && m[2] != "--" {
			if v, err := strconv.ParseUint(m[2], 10, 32); err == nil {
				if v > 100 {
					v = 100
				}
				cur.npuPct, cur.npuOK = uint32(v), true
			}
		}
		if m := axclMemPattern.FindStringSubmatch(cells[2]); m != nil {
			used, e1 := strconv.ParseUint(m[1], 10, 64)
			total, e2 := strconv.ParseUint(m[3], 10, 64)
			if e1 == nil && e2 == nil && axclUnitBytes(m[2]) > 0 && axclUnitBytes(m[4]) > 0 {
				cur.cmmUsed = used * axclUnitBytes(m[2])
				cur.cmmTotal = total * axclUnitBytes(m[4])
				cur.cmmOK = cur.cmmTotal > 0
			}
		}
		cur = nil
	}
	return cards
}

// axeraSnapshot is one published table: the time it was read and, per PCI
// address, the card's name and stat.
type axeraSnapshot struct {
	at    time.Time
	stats map[string]gpuStat
	names map[string]string
}

// axeraSampler runs axcl-smi on its own goroutine, one call at a time (the loop
// is the single-flight guard: a call that overruns delays the next, it never
// overlaps it), and publishes the latest parsed table atomically.
type axeraSampler struct {
	smiPath string
	latest  atomic.Pointer[axeraSnapshot]
	fails   int  // consecutive failed calls; sampler goroutine only
	warned  bool // sampler goroutine only
	stop    chan struct{}
	done    chan struct{}
}

func (s *axeraSampler) sample() {
	ctx, cancel := context.WithTimeout(context.Background(), axeraSMITimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.smiPath)
	// The CLI prints a harmless log-sink permission error on stderr; stdout is
	// the table. WaitDelay bounds the wait if the killed CLI left a child
	// holding the pipe.
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	cards := parseAxclSmiTable(string(out))
	if err != nil || len(cards) == 0 {
		// A second axcl-smi session running at the same moment (an operator at a
		// shell) makes one call return no table; that is not a fault, and the
		// last good table stays published for axeraFreshFor. Warn only when it
		// persists for as long as the table stays fresh.
		s.fails++
		slog.Debug("axcl-smi gave no usable table", "path", s.smiPath, "err", err, "cards", len(cards), "consecutive", s.fails)
		if s.fails >= 3 && !s.warned {
			s.warned = true
			slog.Warn("axcl-smi has given no usable table for several samples; Axera card telemetry is omitted until it does",
				"path", s.smiPath, "err", err, "cards", len(cards))
		}
		return
	}
	s.fails = 0
	if s.warned {
		s.warned = false
		slog.Info("axcl-smi recovered", "cards", len(cards))
	}
	snap := &axeraSnapshot{
		at:    time.Now(),
		stats: make(map[string]gpuStat, len(cards)),
		names: make(map[string]string, len(cards)),
	}
	for _, c := range cards {
		st := gpuStat{UtilizationPct: c.npuPct, UtilizationKnown: c.npuOK}
		if c.tempOK {
			st.TemperatureC = c.tempC
		}
		if c.cmmOK {
			st.VRAMTotal, st.VRAMUsed = c.cmmTotal, c.cmmUsed
		}
		snap.stats[axeraStatsKey(c.busID)] = st
		snap.names[axeraStatsKey(c.busID)] = axeraCardName(c.name)
	}
	s.latest.Store(snap)
}

func (s *axeraSampler) run() {
	defer close(s.done)
	ticker := time.NewTicker(axeraSampleInterval)
	defer ticker.Stop()
	// The first table was taken synchronously by detection, so wait one interval.
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			s.sample()
		}
	}
}

// fresh returns the last good snapshot while it is recent enough to publish.
func (s *axeraSampler) fresh(now time.Time) (*axeraSnapshot, bool) {
	p := s.latest.Load()
	if p == nil || now.Sub(p.at) > axeraFreshFor {
		return nil, false
	}
	return p, true
}

func (s *axeraSampler) Stop() {
	close(s.stop)
	<-s.done
}

// axeraSamplerSet holds the process-wide sampler, started once by detection and
// living as long as the inventory rows it feeds (the process).
var axeraSamplerSet struct {
	mu sync.Mutex
	s  *axeraSampler
}

func runningAxeraSampler() *axeraSampler {
	axeraSamplerSet.mu.Lock()
	defer axeraSamplerSet.mu.Unlock()
	return axeraSamplerSet.s
}

// detectAxeraCards returns one accelerator row per Axera PCI function and, when
// the vendor CLI is installed, starts the sampler. A host without an Axera card
// returns nil without touching the CLI.
func detectAxeraCards() []GPUInfo {
	devs := listAxeraDevices(axeraPCIDevicesDir)
	if len(devs) == 0 {
		return nil
	}
	rows := make([]GPUInfo, 0, len(devs))
	for _, d := range devs {
		rows = append(rows, axeraRow(d, ""))
		slog.Debug("Axera card detected", "bdf", d.bdf, "device", d.vendor+":"+d.device)
	}
	if info, err := os.Stat(axeraSMIPath); err != nil || info.IsDir() {
		slog.Info("Axera card present but axcl-smi is not installed; memory, utilization and temperature will be omitted",
			"path", axeraSMIPath)
		return rows
	}
	axeraSamplerSet.mu.Lock()
	defer axeraSamplerSet.mu.Unlock()
	if axeraSamplerSet.s != nil {
		return rows
	}
	// One synchronous table at startup (~0.6 s, like nvidia-smi's first query)
	// so the rows are named from it and the first response already has numbers.
	s := &axeraSampler{smiPath: axeraSMIPath, stop: make(chan struct{}), done: make(chan struct{})}
	s.sample()
	if snap := s.latest.Load(); snap != nil {
		for i := range rows {
			if name := snap.names[rows[i].statsKey]; name != "" {
				rows[i].Name = name
			}
		}
	}
	axeraSamplerSet.s = s
	go s.run()
	return rows
}

// mergeAxeraStats folds the sampler's last fresh table into the snapshot's GPU
// map under each card's statsKey. Like the other accelerator samplers it leaves
// GPUSampledAt untouched, and it clones the map first because on a stale-preserve
// tick snap.GPU aliases the previously published one.
func mergeAxeraStats(snap *statsSnapshot) {
	s := runningAxeraSampler()
	if s == nil {
		return
	}
	p, ok := s.fresh(time.Now())
	if !ok {
		return
	}
	merged := make(map[string]gpuStat, len(snap.GPU)+len(p.stats))
	for k, v := range snap.GPU {
		merged[k] = v
	}
	for k, v := range p.stats {
		merged[k] = v
	}
	snap.GPU = merged
}
