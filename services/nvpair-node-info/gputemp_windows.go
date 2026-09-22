// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows GPU temperature and power draw.
//
// PDH has neither counter and DXGI reports neither, so the only driverless
// source on an NVIDIA host is nvidia-smi, which keys its rows by PCI bus id.
// The rest of the Windows collector keys GPUs by DXGI adapter LUID (the PDH
// instance form). The bridge between the two is the kernel-mode display
// driver: D3DKMTOpenAdapterFromLuid + D3DKMTQueryAdapterInfo
// (KMTQAITYPE_ADAPTERADDRESS) return the PCI bus/device/function behind a
// LUID, so each nvidia-smi row can be joined to its adapter without guessing
// by name — which matters on a host with two identical cards.
//
// nvidia-smi is a process spawn (~100 ms), so it runs in its own goroutine on
// a slow ticker rather than inside the 1 s PDH tick; the tick merges the
// latest published map. A host without nvidia-smi (AMD / Intel / no NVIDIA
// driver) latches unavailable on the first failure and reports neither
// reading, exactly as before.
//
// Both readings ride the SAME query and the same PCI-address join, because
// they come from the same rows: adding power.draw here costs no extra process
// spawn and cannot drift from the temperature it is displayed beside.
//
// The same row also carries NVML's utilization.gpu. PDH's GPU Engine counters
// miss CUDA work submitted from WSL2 (vLLM, llama.cpp under WSL): measured on
// a three-card host, nvidia-smi said 100 % on every card while PDH said 3 %,
// 0 %, 0 %. mergeInto publishes the busier of the two, so native and WSL
// compute both register and a game on the 3D engine still reads as before.
//
// A failed poll drops the readings instead of serving the previous ones: one
// nvidia-smi timeout under load used to latch the poller off for the life of
// the process while the last sample stayed on screen, which froze a card's
// wattage mid-game. Only a host without nvidia-smi latches off.

const (
	gpuTempPollInterval = 2 * time.Second
	// nvidiaSmiWindowsTimeout bounds one query. nvidia-smi on a card under
	// full load has been measured past the old 3 s bound; the poller runs in
	// its own goroutine, so a slow answer only delays the next reading.
	nvidiaSmiWindowsTimeout = 10 * time.Second
)

// gpuSensorSample is what one nvidia-smi row contributes to a GPU's dynamic
// state — the readings PDH and DXGI cannot supply, plus NVML's utilization.
// A zero temperature or wattage means the card answered [N/A] (several
// virtual and headless SKUs do) and the matching wire field is omitted rather
// than published as a literal zero; a zero utilization is merged as "no
// busier than PDH says", which is the same thing.
type gpuSensorSample struct {
	TemperatureC   uint32
	PowerWatts     float64
	UtilizationPct uint32
}

var (
	modGdi32                      = windows.NewLazySystemDLL("gdi32.dll")
	procD3DKMTOpenAdapterFromLuid = modGdi32.NewProc("D3DKMTOpenAdapterFromLuid")
	procD3DKMTQueryAdapterInfo    = modGdi32.NewProc("D3DKMTQueryAdapterInfo")
	procD3DKMTCloseAdapter        = modGdi32.NewProc("D3DKMTCloseAdapter")
)

// KMTQAITYPE_ADAPTERADDRESS (= 6 in KMTQUERYADAPTERINFOTYPE: UMDRIVERPRIVATE 0,
// UMDRIVERNAME 1, UMOPENGLINFO 2, GETSEGMENTSIZE 3, ADAPTERGUID 4,
// FLIPQUEUEINFO 5, ADAPTERADDRESS 6) selects D3DKMT_ADAPTERADDRESS from
// D3DKMTQueryAdapterInfo.
const kmtQueryAdapterAddress = 6

// d3dkmtOpenAdapterFromLuid mirrors D3DKMT_OPENADAPTERFROMLUID.
type d3dkmtOpenAdapterFromLuid struct {
	LuidLow  uint32
	LuidHigh int32
	HAdapter uint32
}

// d3dkmtQueryAdapterInfo mirrors D3DKMT_QUERYADAPTERINFO (amd64/arm64 layout:
// two uint32, a pointer, a uint32 + padding).
type d3dkmtQueryAdapterInfo struct {
	HAdapter              uint32
	Type                  uint32
	PrivateDriverData     unsafe.Pointer
	PrivateDriverDataSize uint32
	_                     uint32
}

// d3dkmtAdapterAddress mirrors D3DKMT_ADAPTERADDRESS.
type d3dkmtAdapterAddress struct {
	BusNumber      uint32
	DeviceNumber   uint32
	FunctionNumber uint32
}

// pciAddressKey renders a bus/device/function the way nvidia-smi prints
// pci.bus_id after its domain: lowercase "bb:dd.f".
func pciAddressKey(bus, device, function uint32) string {
	return fmt.Sprintf("%02x:%02x.%x", bus&0xff, device, function)
}

// nvidiaBusIDKey reduces an nvidia-smi pci.bus_id ("00000000:65:00.0") to the
// same "bb:dd.f" key. Returns "" when the field is not in that form.
func nvidiaBusIDKey(busID string) string {
	s := strings.ToLower(strings.TrimSpace(busID))
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return ""
	}
	bus, err := strconv.ParseUint(parts[1], 16, 32)
	if err != nil {
		return ""
	}
	devFn := strings.Split(parts[2], ".")
	if len(devFn) != 2 {
		return ""
	}
	dev, err := strconv.ParseUint(devFn[0], 16, 32)
	if err != nil {
		return ""
	}
	fn, err := strconv.ParseUint(devFn[1], 16, 32)
	if err != nil {
		return ""
	}
	return pciAddressKey(uint32(bus), uint32(dev), uint32(fn))
}

// adapterAddressForLuid asks the display driver for the PCI address behind a
// LUID. ok is false when the adapter cannot be opened or has no address (a
// software or remote adapter).
func adapterAddressForLuid(low uint32, high int32) (string, bool) {
	open := d3dkmtOpenAdapterFromLuid{LuidLow: low, LuidHigh: high}
	if st, _, _ := procD3DKMTOpenAdapterFromLuid.Call(uintptr(unsafe.Pointer(&open))); st != 0 {
		return "", false
	}
	defer func() {
		h := open.HAdapter
		procD3DKMTCloseAdapter.Call(uintptr(unsafe.Pointer(&h)))
	}()
	var addr d3dkmtAdapterAddress
	q := d3dkmtQueryAdapterInfo{
		HAdapter:              open.HAdapter,
		Type:                  kmtQueryAdapterAddress,
		PrivateDriverData:     unsafe.Pointer(&addr),
		PrivateDriverDataSize: uint32(unsafe.Sizeof(addr)),
	}
	if st, _, _ := procD3DKMTQueryAdapterInfo.Call(uintptr(unsafe.Pointer(&q))); st != 0 {
		return "", false
	}
	return pciAddressKey(addr.BusNumber, addr.DeviceNumber, addr.FunctionNumber), true
}

// luidsByPCIAddress enumerates the DXGI adapters once more and returns
// PCI address -> PDH LUID key.
//
// It deliberately does NOT apply the DirectX-registry LUID gate that
// detectGPUs runs (selectPhysicalAdapters). This map only answers "which
// adapter is this nvidia-smi row", so an entry nothing ever looks up is
// harmless, while a missing entry silently drops a real card's temperature —
// including for an adapter recovered after a stale-registry boot, whose LUID
// is in this map exactly because the gate was never consulted here. Which
// adapters get reported at all stays detectGPUs' decision.
func luidsByPCIAddress() map[string]string {
	candidates := enumerateAdapterCandidates()
	out := make(map[string]string, len(candidates))
	for _, c := range candidates {
		if addr, ok := adapterAddressForLuid(c.luidLow, c.luidHigh); ok {
			out[addr] = c.gpu.statsKey
		}
	}
	return out
}

// nvidiaSmiSensors runs nvidia-smi and returns PCI address -> readings.
func nvidiaSmiSensors() (map[string]gpuSensorSample, error) {
	ctx, cancel := context.WithTimeout(context.Background(), nvidiaSmiWindowsTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=pci.bus_id,temperature.gpu,power.draw,utilization.gpu",
		"--format=csv,noheader,nounits")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return parseNvidiaSensors(string(out)), nil
}

// parseNvidiaSensors decodes "pci.bus_id, temperature.gpu, power.draw,
// utilization.gpu" rows; the trailing columns are optional.
// A row with an unparseable bus id is skipped entirely; within a row each
// reading is independent, so a card that answers [N/A] for one still
// contributes the other. A row that yields neither is dropped rather than
// stored as a pair of zeroes, which would read as a cold, idle card.
func parseNvidiaSensors(out string) map[string]gpuSensorSample {
	res := map[string]gpuSensorSample{}
	for _, line := range strings.Split(out, "\n") {
		fields := splitCSVRow(line)
		if len(fields) < 2 {
			continue
		}
		key := nvidiaBusIDKey(fields[0])
		if key == "" {
			continue
		}
		var sample gpuSensorSample
		if c, err := strconv.ParseUint(fields[1], 10, 32); err == nil {
			sample.TemperatureC = uint32(c)
		}
		if len(fields) >= 3 {
			if w, ok := parseWatts(fields[2]); ok {
				sample.PowerWatts = w
			}
		}
		if len(fields) >= 4 {
			if u, err := strconv.ParseUint(fields[3], 10, 32); err == nil {
				sample.UtilizationPct = uint32(min(u, 100))
			}
		}
		if sample == (gpuSensorSample{}) {
			continue
		}
		res[key] = sample
	}
	return res
}

// gpuTempPoller owns the slow nvidia-smi loop and publishes LUID key ->
// readings atomically for the PDH tick to merge.
type gpuTempPoller struct {
	// byAddress maps PCI address -> LUID key. Resolved at start and re-resolved
	// by poll() when a reading arrives for an address it does not know; only the
	// poll goroutine ever touches it.
	byAddress   map[string]string
	latest      atomic.Pointer[map[string]gpuSensorSample]
	unavailable atomic.Bool
	// query runs one nvidia-smi read; nvidiaSmiSensors unless a test swaps it.
	query func() (map[string]gpuSensorSample, error)
	// failing is true while consecutive polls fail, so a failure streak logs
	// once and its end logs once. Only the poll goroutine touches it.
	failing bool
	stop        chan struct{}
	done        chan struct{}
}

func startGPUTempPoller() *gpuTempPoller {
	p := &gpuTempPoller{
		byAddress: luidsByPCIAddress(),
		query:     nvidiaSmiSensors,
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
	if len(p.byAddress) == 0 {
		slog.Info("no adapter PCI addresses resolvable yet; retrying when a temperature reading arrives")
	}
	go p.run()
	return p
}

func (p *gpuTempPoller) run() {
	defer close(p.done)
	p.poll()
	ticker := time.NewTicker(gpuTempPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-ticker.C:
			p.poll()
		}
	}
}

// poll reads nvidia-smi and republishes the LUID-keyed reading map.
//
// byAddress is resolved once at start because LUIDs are fixed for the life of
// a boot, but a reading can still arrive for an address it does not know: the
// display driver may not have been ready when the poller started, or an
// adapter appeared later (hot-plug, or a GPU recovered by the inventory
// retry). One re-resolve per poll covers both without re-enumerating DXGI on
// every tick.
func (p *gpuTempPoller) poll() {
	if p.unavailable.Load() {
		return
	}
	sensors, err := p.query()
	if err != nil {
		// Never keep serving the previous sample: a stale wattage renders as a
		// live one. The fields go absent until a poll succeeds again.
		p.latest.Store(nil)
		if errors.Is(err, exec.ErrNotFound) {
			p.unavailable.Store(true)
			slog.Warn("nvidia-smi not found; GPU temperature, power draw and NVML utilization will not be reported", "err", err)
			return
		}
		if !p.failing {
			p.failing = true
			slog.Warn("nvidia-smi query failed; GPU temperature, power draw and NVML utilization omitted until it answers again", "err", err)
		}
		return
	}
	if p.failing {
		p.failing = false
		slog.Info("nvidia-smi answering again; GPU temperature, power draw and NVML utilization restored")
	}
	byLUID := make(map[string]gpuSensorSample, len(sensors))
	reResolved := false
	for addr, sample := range sensors {
		luid, ok := p.byAddress[addr]
		if !ok && !reResolved {
			reResolved = true
			p.byAddress = luidsByPCIAddress()
			luid, ok = p.byAddress[addr]
		}
		if ok {
			byLUID[luid] = sample
		}
	}
	p.latest.Store(&byLUID)
}

// mergeInto adds the latest readings to the snapshot's GPU map under their
// LUID keys, cloning the map first so a previously published snapshot (which
// the stale-preserve path may alias) is never mutated.
//
// Each field is copied only when the card reported it, so a row whose
// temperature came through as [N/A] keeps whatever the rest of the collector
// knows rather than having it overwritten with a zero.
func (p *gpuTempPoller) mergeInto(snap *statsSnapshot) {
	if p == nil {
		return
	}
	sensors := p.latest.Load()
	if sensors == nil || len(*sensors) == 0 {
		return
	}
	merged := make(map[string]gpuStat, len(snap.GPU)+len(*sensors))
	for k, v := range snap.GPU {
		merged[k] = v
	}
	for luid, sample := range *sensors {
		s := merged[luid]
		if sample.TemperatureC > 0 {
			s.TemperatureC = sample.TemperatureC
		}
		if sample.PowerWatts > 0 {
			s.PowerWatts = sample.PowerWatts
		}
		s.UtilizationPct = max(s.UtilizationPct, sample.UtilizationPct)
		merged[luid] = s
	}
	snap.GPU = merged
}

func (p *gpuTempPoller) Stop() {
	if p == nil {
		return
	}
	close(p.stop)
	<-p.done
}
