// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nvpair-shared/noderec"
)

// The amdgpu inventory and sampler are pure sysfs readers, so every case below
// runs against a fake tree under t.TempDir() shaped exactly like the real
// class: <root>/drm holds the card<N> entries whose `device` is a symlink into
// <root>/pci/<address>, which is where the attributes actually live. The
// symlink is real (not a copied directory) because the PCI address in every
// statsKey comes from resolving it.
//
// The numbers used are the ones measured on a Ryzen 5 5625U APU (device 0x15e7,
// 512 MiB VRAM carve-out, 15.1 GiB GTT, edge sensor at 67 C).

const (
	fakeAPUVRAMTotal = 536870912   // 512 MiB carve-out
	fakeAPUGTTTotal  = 16239468544 // 15.12 GiB aperture
)

// amdFakeTree is a sysfs-shaped temp tree.
type amdFakeTree struct {
	t       *testing.T
	drmRoot string
	pciRoot string
}

func newAMDFakeTree(t *testing.T) *amdFakeTree {
	t.Helper()
	root := t.TempDir()
	f := &amdFakeTree{
		t:       t,
		drmRoot: filepath.Join(root, "drm"),
		pciRoot: filepath.Join(root, "pci"),
	}
	mkdir(t, f.drmRoot)
	mkdir(t, f.pciRoot)
	return f
}

// addCard creates <drm>/<card> whose `device` symlinks to <pci>/<pciAddr>,
// populated with attrs (values are written with the trailing newline the
// kernel appends). It returns the device directory.
func (f *amdFakeTree) addCard(card, pciAddr string, attrs map[string]string) string {
	f.t.Helper()
	deviceDir := filepath.Join(f.pciRoot, pciAddr)
	mkdir(f.t, deviceDir)
	writeAttrs(f.t, deviceDir, attrs)
	cardDir := filepath.Join(f.drmRoot, card)
	mkdir(f.t, cardDir)
	if err := os.Symlink(deviceDir, filepath.Join(cardDir, "device")); err != nil {
		f.t.Fatalf("symlink %s device: %v", card, err)
	}
	return deviceDir
}

// addPlainCard creates <drm>/<card>/device as a real directory, standing in
// for a sysfs view where the link cannot be resolved.
func (f *amdFakeTree) addPlainCard(card string, attrs map[string]string) string {
	f.t.Helper()
	deviceDir := filepath.Join(f.drmRoot, card, "device")
	mkdir(f.t, deviceDir)
	writeAttrs(f.t, deviceDir, attrs)
	return deviceDir
}

// addEntry creates a non-card class entry (a connector or render node) that
// carries an AMD vendor id, which is how the real class looks.
func (f *amdFakeTree) addEntry(name string, attrs map[string]string) {
	f.t.Helper()
	deviceDir := filepath.Join(f.drmRoot, name, "device")
	mkdir(f.t, deviceDir)
	writeAttrs(f.t, deviceDir, attrs)
}

// addHwmon creates <deviceDir>/hwmon/<hwmon> with the given files.
func (f *amdFakeTree) addHwmon(deviceDir, hwmon string, files map[string]string) {
	f.t.Helper()
	dir := filepath.Join(deviceDir, "hwmon", hwmon)
	mkdir(f.t, dir)
	writeAttrs(f.t, dir, files)
}

// addGCIPVersion creates the amdgpu IP-discovery node that reports the card's
// Graphics Core IP version, at the path the driver really uses. The measured
// Barcelo APU reports 9.3.0 there, which is gfx90c.
func (f *amdFakeTree) addGCIPVersion(deviceDir, major, minor, revision string) {
	f.t.Helper()
	dir := filepath.Join(deviceDir, amdGCIPPath)
	mkdir(f.t, dir)
	writeAttrs(f.t, dir, map[string]string{
		"major": major, "minor": minor, "revision": revision,
	})
}

func mkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func writeAttrs(t *testing.T, dir string, attrs map[string]string) {
	t.Helper()
	for name, value := range attrs {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value+"\n"), 0o644); err != nil {
			t.Fatalf("write %s/%s: %v", dir, name, err)
		}
	}
}

// apuAttrs is the measured Barcelo APU attribute set, with overrides applied.
func apuAttrs(overrides map[string]string) map[string]string {
	attrs := map[string]string{
		"vendor":                  "0x1002",
		"device":                  "0x15e7",
		"mem_info_vram_total":     "536870912",
		"mem_info_vram_used":      "42164224",
		"mem_info_gtt_total":      "16239468544",
		"mem_info_gtt_used":       "12816384",
		"mem_info_vis_vram_total": "536870912",
		"gpu_busy_percent":        "0",
		"uevent": strings.Join([]string{
			"DRIVER=amdgpu",
			"PCI_CLASS=30000",
			"PCI_ID=1002:15E7",
			"PCI_SLOT_NAME=0000:04:00.0",
		}, "\n"),
	}
	for k, v := range overrides {
		attrs[k] = v
	}
	return attrs
}

// TestDetectAMDGPUsAPUCapacityIsVRAMPlusGTT is the headline rule: a firmware
// carve-out of 512 MiB is not what the machine can run, so the reported
// capacity is the carve-out plus the GTT aperture. Reporting the carve-out
// alone would have a scheduler rule this node out of every real model.
func TestDetectAMDGPUsAPUCapacityIsVRAMPlusGTT(t *testing.T) {
	f := newAMDFakeTree(t)
	f.addCard("card1", "0000:04:00.0", apuAttrs(nil))

	gpus := detectAMDGPUs(f.drmRoot)
	if len(gpus) != 1 {
		t.Fatalf("detectAMDGPUs = %d rows, want 1: %+v", len(gpus), gpus)
	}
	got := gpus[0]
	if want := uint64(fakeAPUVRAMTotal + fakeAPUGTTTotal); got.VramBytes != want {
		t.Errorf("VramBytes = %d, want %d (vram+gtt)", got.VramBytes, want)
	}
	if want := "AMD Radeon Vega Graphics (Barcelo, GCN 5.1)"; got.Name != want {
		t.Errorf("Name = %q, want %q", got.Name, want)
	}
	if want := "amd:0000:04:00.0"; got.statsKey != want {
		t.Errorf("statsKey = %q, want %q", got.statsKey, want)
	}
	if got.Kind != "" {
		t.Errorf("Kind = %q, want empty (an APU is a GPU, not an accelerator)", got.Kind)
	}
	if got.usesSystemMemoryUsage {
		t.Error("usesSystemMemoryUsage set: amdgpu reports its own usage, /proc/meminfo must not override it")
	}
	// The capacity is a pool the CPU shares, so the row says so — and unlike
	// the sysfs-only unified rows this one still publishes a used figure,
	// because the driver measures vram_used + gtt_used for this device.
	if got.MemoryPool != noderec.GPUMemoryPoolUnified {
		t.Errorf("MemoryPool = %q, want %q: an APU's capacity is carve-out + GTT out of system RAM",
			got.MemoryPool, noderec.GPUMemoryPoolUnified)
	}
}

// TestDetectAMDGPUsAPURowKeepsItsOwnUsedFigure is the APU's place in the
// unified-memory contract, asserted on the wire. Every other shared-pool row
// in this service omits vram_used_bytes because nothing measures it; the APU
// is the exception that must NOT be swept up with them, and the number it
// publishes has to be the driver's rather than the host's.
func TestDetectAMDGPUsAPURowKeepsItsOwnUsedFigure(t *testing.T) {
	f := newAMDFakeTree(t)
	f.addCard("card1", "0000:04:00.0", apuAttrs(nil))

	gpus := detectAMDGPUs(f.drmRoot)
	if len(gpus) != 1 {
		t.Fatalf("detectAMDGPUs = %d rows, want 1", len(gpus))
	}
	const driverUsed uint64 = 3 << 30
	const hostUsed uint64 = 25_500_000_000
	snap := statsSnapshot{
		MemUsedBytes: hostUsed,
		GPU:          map[string]gpuStat{gpus[0].statsKey: {VRAMUsed: driverUsed, UtilizationPct: 12}},
	}
	var raw map[string]any
	if err := json.Unmarshal(buildResponse(gpus, nil, 0, snap, "", nil), &raw); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	row := raw["GPUs"].([]any)[0].(map[string]any)
	if got := row["memory_pool"]; got != noderec.GPUMemoryPoolUnified {
		t.Errorf("memory_pool = %v, want %q", got, noderec.GPUMemoryPoolUnified)
	}
	used, present := row["vram_used_bytes"]
	if !present {
		t.Fatalf("vram_used_bytes absent: amdgpu measured %d and the row must carry it", driverUsed)
	}
	if got := uint64(used.(float64)); got != driverUsed {
		t.Errorf("vram_used_bytes = %d, want the driver's %d (the host's is %d)", got, driverUsed, hostUsed)
	}
}

// TestDetectAMDGPUsDiscreteCapacityIsVRAMOnly pins the other half of the rule.
// A discrete card also exposes mem_info_gtt_total; adding it would inflate a
// 16 GiB card to 24 GiB.
func TestDetectAMDGPUsDiscreteCapacityIsVRAMOnly(t *testing.T) {
	f := newAMDFakeTree(t)
	f.addCard("card0", "0000:03:00.0", map[string]string{
		"vendor":              "0x1002",
		"device":              "0x73bf", // Navi 21, not in amdModels
		"mem_info_vram_total": "17163091968",
		"mem_info_gtt_total":  "8589934592",
	})

	gpus := detectAMDGPUs(f.drmRoot)
	if len(gpus) != 1 {
		t.Fatalf("detectAMDGPUs = %d rows, want 1", len(gpus))
	}
	if got, want := gpus[0].VramBytes, uint64(17163091968); got != want {
		t.Errorf("VramBytes = %d, want %d (vram only)", got, want)
	}
	if got, want := gpus[0].Name, "AMD Radeon Graphics (device 0x73bf)"; got != want {
		t.Errorf("Name = %q, want %q", got, want)
	}
}

// TestDetectAMDGPUsUnlistedAPUIsUnifiedByItsNumbers covers a part released
// after amdModels was written: a sub-1 GiB VRAM figure next to a GTT aperture
// is a carve-out, whatever the device id.
func TestDetectAMDGPUsUnlistedAPUIsUnifiedByItsNumbers(t *testing.T) {
	f := newAMDFakeTree(t)
	f.addCard("card1", "0000:04:00.0", map[string]string{
		"vendor":              "0x1002",
		"device":              "0x9999", // unlisted
		"mem_info_vram_total": "536870912",
		"mem_info_gtt_total":  "8589934592",
	})

	gpus := detectAMDGPUs(f.drmRoot)
	if len(gpus) != 1 {
		t.Fatalf("detectAMDGPUs = %d rows, want 1", len(gpus))
	}
	if got, want := gpus[0].VramBytes, uint64(536870912+8589934592); got != want {
		t.Errorf("VramBytes = %d, want %d (vram+gtt)", got, want)
	}
}

// TestAMDModelName pins every table entry and both fallbacks. Each name
// carries the graphics generation, because that is the question a node list
// has to answer ("Barcelo" alone says nothing about what the part can run),
// and each was checked against the kernel's amdgpu PCI table, pci.ids and
// libdrm's amdgpu.ids rather than recalled. The fallback exists so an unknown
// card is still published under a name a user recognizes as their GPU - never
// a bare codename, never a dropped row.
func TestAMDModelName(t *testing.T) {
	cases := []struct{ id, want string }{
		{"15dd", "AMD Radeon Vega Graphics (Raven Ridge, GCN 5)"},
		{"15d8", "AMD Radeon Vega Graphics (Picasso, GCN 5)"},
		{"1636", "AMD Radeon Vega Graphics (Renoir, GCN 5.1)"},
		{"164c", "AMD Radeon Vega Graphics (Lucienne, GCN 5.1)"},
		{"1638", "AMD Radeon Vega Graphics (Cezanne, GCN 5.1)"},
		{"15e7", "AMD Radeon Vega Graphics (Barcelo, GCN 5.1)"},
		{"164e", "AMD Radeon Graphics (Raphael, RDNA 2)"},
		{"1681", "AMD Radeon 680M (Rembrandt, RDNA 2)"},
		{"15bf", "AMD Radeon 780M (Phoenix, RDNA 3)"},
		{"15c8", "AMD Radeon 740M (Phoenix2, RDNA 3)"},
		{"1900", "AMD Radeon 780M (Hawk Point, RDNA 3)"},
		{"150e", "AMD Radeon 890M (Strix Point, RDNA 3.5)"},
		{"1586", "AMD Radeon 8060S (Strix Halo, RDNA 3.5)"},
		{"1114", "AMD Radeon 860M (Krackan Point, RDNA 3.5)"},
		{"7480", "AMD Radeon Graphics (device 0x7480)"},
		{"", "AMD Radeon Graphics"},
	}
	for _, c := range cases {
		// No device directory: the table and the bare fallback are the only
		// sources, which is the case on every host with no IP-discovery node.
		if got := amdModelName(c.id, ""); got != c.want {
			t.Errorf("amdModelName(%q) = %q, want %q", c.id, got, c.want)
		}
	}
}

// TestAMDModelNameNamesTheGenerationAndNeverTheCoreCount is the rule the table
// exists to enforce. The compute-unit count is NOT derivable from the device
// id - 0x15e7 alone ships as Vega 6, Vega 7 and Vega 8, separated only by the
// PCI revision id - so printing one is a guess that is wrong on most SKUs. The
// generation is derivable, and is what the operator asked to see.
func TestAMDModelNameNamesTheGenerationAndNeverTheCoreCount(t *testing.T) {
	architectures := []string{"GCN 5", "GCN 5.1", "RDNA 2", "RDNA 3", "RDNA 3.5"}
	for id, model := range amdModels {
		name := amdModelName(id, "")
		named := false
		for _, arch := range architectures {
			if strings.HasSuffix(name, ", "+arch+")") {
				named = true
				break
			}
		}
		if !named {
			t.Errorf("amdModels[%q] = %q; every name must end in a known architecture %v", id, name, architectures)
		}
		if !strings.Contains(name, " (") {
			t.Errorf("amdModels[%q] = %q; want the \"<name> (<codename>, <architecture>)\" shape", id, name)
		}
		// "Vega 7" and friends are core counts, not generations. "Vega
		// Graphics" (no digit) is the marketing name AMD itself uses for the
		// whole family and is allowed.
		for _, banned := range []string{"Vega 3", "Vega 6", "Vega 7", "Vega 8", "Vega 10", "Vega 11", " CU", "cores"} {
			if strings.Contains(name, banned) {
				t.Errorf("amdModels[%q] = %q contains %q: a compute-unit count cannot be derived from a device id", id, name, banned)
			}
		}
		if !model.apu {
			t.Errorf("amdModels[%q] is not marked apu; every listed part is an integrated GPU", id)
		}
	}
}

// TestAMDModelNameFallbackDerivesArchitectureFromGCIP covers a part released
// after amdModels was written. amdgpu publishes the ASIC's own Graphics Core
// IP version in sysfs, so the generation is still knowable even when the name
// is not - and a row reading "(device 0x1234, RDNA 3.5)" is useful on day one,
// where a bare id is not.
func TestAMDModelNameFallbackDerivesArchitectureFromGCIP(t *testing.T) {
	cases := []struct {
		name                   string
		major, minor, revision string
		want                   string
	}{
		{"Renoir-class gfx90c", "9", "3", "0", "AMD Radeon Graphics (device 0x1234, GCN 5.1)"},
		{"Vega", "9", "1", "0", "AMD Radeon Graphics (device 0x1234, GCN 5)"},
		{"RDNA", "10", "1", "10", "AMD Radeon Graphics (device 0x1234, RDNA)"},
		{"RDNA 2", "10", "3", "1", "AMD Radeon Graphics (device 0x1234, RDNA 2)"},
		{"RDNA 3", "11", "0", "4", "AMD Radeon Graphics (device 0x1234, RDNA 3)"},
		{"RDNA 3.5", "11", "5", "0", "AMD Radeon Graphics (device 0x1234, RDNA 3.5)"},
		{"RDNA 4", "12", "0", "1", "AMD Radeon Graphics (device 0x1234, RDNA 4)"},
		// 9.4.x is the data-center CDNA line, not GCN 5.4. Naming it from the
		// "9.x is GCN 5.x" shorthand would be wrong, so the architecture is
		// dropped instead of invented.
		{"unmapped CDNA", "9", "4", "3", "AMD Radeon Graphics (device 0x1234)"},
		{"unmapped future", "13", "0", "0", "AMD Radeon Graphics (device 0x1234)"},
		{"unparseable", "N/A", "", "", "AMD Radeon Graphics (device 0x1234)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newAMDFakeTree(t)
			dev := f.addCard("card1", "0000:04:00.0", map[string]string{
				"vendor": "0x1002", "device": "0x1234",
			})
			f.addGCIPVersion(dev, c.major, c.minor, c.revision)

			if got := amdModelName("1234", dev); got != c.want {
				t.Errorf("amdModelName = %q, want %q", got, c.want)
			}
			gpus := detectAMDGPUs(f.drmRoot)
			if len(gpus) != 1 {
				t.Fatalf("detectAMDGPUs = %d rows, want 1", len(gpus))
			}
			if gpus[0].Name != c.want {
				t.Errorf("row Name = %q, want %q (the detector must use the same name)", gpus[0].Name, c.want)
			}
		})
	}
}

// TestAMDModelNameFallbackWithoutGCIP: every pre-Renoir part has no
// ip_discovery tree at all, and a missing tree must not be an error - it just
// leaves the architecture out.
func TestAMDModelNameFallbackWithoutGCIP(t *testing.T) {
	f := newAMDFakeTree(t)
	dev := f.addCard("card1", "0000:04:00.0", map[string]string{
		"vendor": "0x1002", "device": "0x1234",
	})
	if got, want := amdModelName("1234", dev), "AMD Radeon Graphics (device 0x1234)"; got != want {
		t.Errorf("amdModelName = %q, want %q", got, want)
	}
}

// TestAMDGCArchitectureIsNotConsultedForAListedPart: the table is
// authoritative. A listed id keeps its verified name even if the card's IP
// version maps elsewhere, so the published name never depends on which of two
// sources happened to be readable.
func TestAMDGCArchitectureIsNotConsultedForAListedPart(t *testing.T) {
	f := newAMDFakeTree(t)
	dev := f.addCard("card1", "0000:04:00.0", apuAttrs(nil))
	f.addGCIPVersion(dev, "12", "0", "0") // RDNA 4, which a Barcelo is not

	if got, want := amdModelName("15e7", dev), "AMD Radeon Vega Graphics (Barcelo, GCN 5.1)"; got != want {
		t.Errorf("amdModelName = %q, want %q", got, want)
	}
}

// TestDetectAMDGPUsSkipsNonAMDVendors keeps this path off cards that already
// have an owner: an NVIDIA card is nvidia-smi's, an Intel one is ghw's.
func TestDetectAMDGPUsSkipsNonAMDVendors(t *testing.T) {
	f := newAMDFakeTree(t)
	f.addCard("card0", "0000:01:00.0", map[string]string{
		"vendor": "0x10de", "device": "0x2504", "mem_info_vram_total": "12884901888",
	})
	f.addCard("card1", "0000:00:02.0", map[string]string{
		"vendor": "0x8086", "device": "0x9a49",
	})

	if gpus := detectAMDGPUs(f.drmRoot); len(gpus) != 0 {
		t.Fatalf("detectAMDGPUs = %+v, want none", gpus)
	}
}

// TestDetectAMDGPUsIgnoresConnectorDirs is the duplicate-row guard: the DRM
// class carries one card<N>-<CONNECTOR> entry per output plus render nodes,
// all pointing back at the same device. The measured host has four.
func TestDetectAMDGPUsIgnoresConnectorDirs(t *testing.T) {
	f := newAMDFakeTree(t)
	f.addCard("card1", "0000:04:00.0", apuAttrs(nil))
	for _, name := range []string{"card1-DP-1", "card1-DP-2", "card1-HDMI-A-1", "card1-HDMI-A-2", "renderD128"} {
		f.addEntry(name, map[string]string{"vendor": "0x1002", "device": "0x15e7"})
	}

	gpus := detectAMDGPUs(f.drmRoot)
	if len(gpus) != 1 {
		t.Fatalf("detectAMDGPUs = %d rows, want 1 (connectors must not list the GPU again): %+v", len(gpus), gpus)
	}
}

// TestDetectAMDGPUsOrdersByCardNumber pins a stable inventory order across
// boots; lexical order would put card10 before card2.
func TestDetectAMDGPUsOrdersByCardNumber(t *testing.T) {
	f := newAMDFakeTree(t)
	f.addCard("card10", "0000:0a:00.0", map[string]string{"vendor": "0x1002", "device": "0x1638"})
	f.addCard("card2", "0000:02:00.0", map[string]string{"vendor": "0x1002", "device": "0x164c"})

	gpus := detectAMDGPUs(f.drmRoot)
	if len(gpus) != 2 {
		t.Fatalf("detectAMDGPUs = %d rows, want 2", len(gpus))
	}
	if gpus[0].statsKey != "amd:0000:02:00.0" || gpus[1].statsKey != "amd:0000:0a:00.0" {
		t.Fatalf("order = %q, %q; want card2 then card10", gpus[0].statsKey, gpus[1].statsKey)
	}
}

// TestAMDStatsKeyFromUeventWhenDeviceIsNotASymlink exercises the fallback for
// a sysfs view where the link cannot be resolved: PCI_SLOT_NAME carries the
// same address, so the key is identical and a row still joins its samples.
func TestAMDStatsKeyFromUeventWhenDeviceIsNotASymlink(t *testing.T) {
	f := newAMDFakeTree(t)
	f.addPlainCard("card1", apuAttrs(nil))

	gpus := detectAMDGPUs(f.drmRoot)
	if len(gpus) != 1 {
		t.Fatalf("detectAMDGPUs = %d rows, want 1", len(gpus))
	}
	if got, want := gpus[0].statsKey, "amd:0000:04:00.0"; got != want {
		t.Errorf("statsKey = %q, want %q (from uevent PCI_SLOT_NAME)", got, want)
	}
}

// TestAMDStatsKeyFallsBackToCardName: with neither a resolvable link nor a
// usable PCI_SLOT_NAME the row still gets a key, because a keyless row loses
// every dynamic field.
func TestAMDStatsKeyFallsBackToCardName(t *testing.T) {
	f := newAMDFakeTree(t)
	f.addPlainCard("card1", map[string]string{
		"vendor": "0x1002",
		"device": "0x15e7",
		"uevent": "DRIVER=amdgpu\nPCI_SLOT_NAME=not-an-address",
	})

	gpus := detectAMDGPUs(f.drmRoot)
	if len(gpus) != 1 {
		t.Fatalf("detectAMDGPUs = %d rows, want 1", len(gpus))
	}
	if got, want := gpus[0].statsKey, "amd:card1"; got != want {
		t.Errorf("statsKey = %q, want %q", got, want)
	}
}

// TestDecodeAMDSamplesAPU checks the per-tick numbers on a unified part: used
// memory is vram_used + gtt_used (the same pool the capacity counted), the
// busy percentage passes through, and the edge sensor's millidegrees become
// whole degrees. A usable utilization reading marks the snapshot sampled.
func TestDecodeAMDSamplesAPU(t *testing.T) {
	f := newAMDFakeTree(t)
	dev := f.addCard("card1", "0000:04:00.0", apuAttrs(map[string]string{
		"gpu_busy_percent":   "42",
		"mem_info_vram_used": "42164224",
		"mem_info_gtt_used":  "12816384",
	}))
	f.addHwmon(dev, "hwmon2", map[string]string{
		"name": "amdgpu", "temp1_input": "67000", "temp1_label": "edge",
	})

	out := map[string]gpuStat{}
	if !decodeAMD(f.drmRoot, out) {
		t.Fatal("decodeAMD reported no sample; a valid gpu_busy_percent is fresh telemetry")
	}
	got, ok := out["amd:0000:04:00.0"]
	if !ok {
		t.Fatalf("no sample under the inventory key; got %v", out)
	}
	if want := uint64(42164224 + 12816384); got.VRAMUsed != want {
		t.Errorf("VRAMUsed = %d, want %d (vram_used+gtt_used)", got.VRAMUsed, want)
	}
	if got.UtilizationPct != 42 {
		t.Errorf("UtilizationPct = %d, want 42", got.UtilizationPct)
	}
	if got.TemperatureC != 67 {
		t.Errorf("TemperatureC = %d, want 67", got.TemperatureC)
	}
}

// TestDecodeAMDDiscreteUsedExcludesGTT: a discrete card's capacity excluded
// the GTT, so its usage must too, or used can exceed total.
func TestDecodeAMDDiscreteUsedExcludesGTT(t *testing.T) {
	f := newAMDFakeTree(t)
	f.addCard("card0", "0000:03:00.0", map[string]string{
		"vendor": "0x1002", "device": "0x73bf",
		"mem_info_vram_total": "17163091968", "mem_info_vram_used": "2147483648",
		"mem_info_gtt_total": "8589934592", "mem_info_gtt_used": "1073741824",
		"gpu_busy_percent": "7",
	})

	out := map[string]gpuStat{}
	if !decodeAMD(f.drmRoot, out) {
		t.Fatal("decodeAMD reported no sample")
	}
	if got, want := out["amd:0000:03:00.0"].VRAMUsed, uint64(2147483648); got != want {
		t.Errorf("VRAMUsed = %d, want %d (vram_used only)", got, want)
	}
}

// TestDecodeAMDInvalidBusyIsSkipped: a driver that cannot answer must not look
// idle, and must not make stale telemetry look fresh. The other two figures
// still publish.
func TestDecodeAMDInvalidBusyIsSkipped(t *testing.T) {
	for _, busy := range []string{"N/A", "101", "", "-1"} {
		t.Run("busy="+busy, func(t *testing.T) {
			f := newAMDFakeTree(t)
			dev := f.addCard("card1", "0000:04:00.0", apuAttrs(map[string]string{
				"gpu_busy_percent": busy,
			}))
			f.addHwmon(dev, "hwmon2", map[string]string{
				"name": "amdgpu", "temp1_input": "67000", "temp1_label": "edge",
			})

			out := map[string]gpuStat{}
			if decodeAMD(f.drmRoot, out) {
				t.Fatalf("decodeAMD reported a sample for gpu_busy_percent=%q", busy)
			}
			got := out["amd:0000:04:00.0"]
			if got.UtilizationPct != 0 {
				t.Errorf("UtilizationPct = %d, want 0", got.UtilizationPct)
			}
			if got.VRAMUsed == 0 || got.TemperatureC != 67 {
				t.Errorf("memory/temperature dropped with utilization: %+v", got)
			}
		})
	}
}

// TestDecodeAMDWithoutAMDGPUIsANoOp is the cost of this feature on every host
// that has no AMD card: nothing published, nothing logged, no error.
func TestDecodeAMDWithoutAMDGPUIsANoOp(t *testing.T) {
	f := newAMDFakeTree(t)
	f.addCard("card0", "0000:01:00.0", map[string]string{"vendor": "0x10de", "device": "0x2504"})

	out := map[string]gpuStat{}
	if decodeAMD(f.drmRoot, out) || len(out) != 0 {
		t.Fatalf("decodeAMD on an NVIDIA-only host published %v", out)
	}
	if decodeAMD(filepath.Join(f.drmRoot, "does-not-exist"), out) || len(out) != 0 {
		t.Fatalf("decodeAMD on a host without /sys/class/drm published %v", out)
	}
	if gpus := detectAMDGPUs(filepath.Join(f.drmRoot, "does-not-exist")); gpus != nil {
		t.Fatalf("detectAMDGPUs on a host without /sys/class/drm = %+v", gpus)
	}
}

// TestAMDEdgeTempPrefersTheEdgeSensor: amdgpu also exposes junction (hotspot)
// and memory sensors that read far hotter. Picking one of those would make an
// AMD row look alarming next to an NVIDIA row reporting its edge temperature.
func TestAMDEdgeTempPrefersTheEdgeSensor(t *testing.T) {
	f := newAMDFakeTree(t)
	dev := f.addCard("card1", "0000:04:00.0", apuAttrs(nil))
	f.addHwmon(dev, "hwmon2", map[string]string{
		"name":        "amdgpu",
		"temp1_input": "95000", "temp1_label": "junction",
		"temp2_input": "67000", "temp2_label": "edge",
		"temp3_input": "88000", "temp3_label": "mem",
	})

	got, ok := amdEdgeTempC(dev)
	if !ok || got != 67 {
		t.Fatalf("amdEdgeTempC = %d, %v; want 67, true", got, ok)
	}
}

// TestAMDEdgeTempFallsBackToFirstSensor covers an unlabelled hwmon: temp1 is
// the edge sensor on amdgpu. The 45.5 C input also pins the rounding.
func TestAMDEdgeTempFallsBackToFirstSensor(t *testing.T) {
	f := newAMDFakeTree(t)
	dev := f.addCard("card1", "0000:04:00.0", apuAttrs(nil))
	f.addHwmon(dev, "hwmon2", map[string]string{
		"name": "amdgpu", "temp1_input": "45500", "temp10_input": "99000",
	})

	got, ok := amdEdgeTempC(dev)
	if !ok || got != 46 {
		t.Fatalf("amdEdgeTempC = %d, %v; want 46, true (temp1 before temp10, rounded)", got, ok)
	}
}

// TestAMDEdgeTempUnavailable: no hwmon, and a hwmon whose reading is garbage,
// both report unavailable so temperature_celsius drops from the JSON instead
// of showing a misleading 0.
func TestAMDEdgeTempUnavailable(t *testing.T) {
	f := newAMDFakeTree(t)
	bare := f.addCard("card1", "0000:04:00.0", apuAttrs(nil))
	if got, ok := amdEdgeTempC(bare); ok {
		t.Errorf("amdEdgeTempC with no hwmon = %d, true; want unavailable", got)
	}

	broken := f.addCard("card2", "0000:05:00.0", apuAttrs(nil))
	f.addHwmon(broken, "hwmon3", map[string]string{
		"name": "amdgpu", "temp1_input": "not-a-number", "temp1_label": "edge",
	})
	if got, ok := amdEdgeTempC(broken); ok {
		t.Errorf("amdEdgeTempC with an unparseable input = %d, true; want unavailable", got)
	}
}

// TestAMDInventoryAndSamplesJoinThroughBuildResponse is the contract that
// matters end to end: the key the inventory stamps and the key the sampler
// publishes under are produced by the same code, so the dynamic fields land on
// the row and the node reports valid telemetry.
func TestAMDInventoryAndSamplesJoinThroughBuildResponse(t *testing.T) {
	f := newAMDFakeTree(t)
	dev := f.addCard("card1", "0000:04:00.0", apuAttrs(map[string]string{"gpu_busy_percent": "13"}))
	f.addHwmon(dev, "hwmon2", map[string]string{
		"name": "amdgpu", "temp1_input": "67000", "temp1_label": "edge",
	})

	gpus := detectAMDGPUs(f.drmRoot)
	sampled := map[string]gpuStat{}
	if !decodeAMD(f.drmRoot, sampled) {
		t.Fatal("decodeAMD reported no sample")
	}
	now := time.Unix(1_700_000_000, 0)
	body := buildResponseAt(gpus, nil, 0, statsSnapshot{
		GPU:          sampled,
		GPUSampledAt: now,
	}, "", nil, now)

	var typed NodeInfoResponse
	if err := json.Unmarshal(body, &typed); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !typed.TelemetryValid {
		t.Error("TelemetryValid = false; an amdgpu utilization sample is GPU telemetry")
	}
	if len(typed.GPUs) != 1 {
		t.Fatalf("GPUs = %d rows, want 1", len(typed.GPUs))
	}
	row := typed.GPUs[0]
	if row.VramBytes != fakeAPUVRAMTotal+fakeAPUGTTTotal {
		t.Errorf("vram_bytes = %d, want %d", row.VramBytes, uint64(fakeAPUVRAMTotal+fakeAPUGTTTotal))
	}
	if row.VramUsedBytes != 42164224+12816384 {
		t.Errorf("vram_used_bytes = %d, want %d", row.VramUsedBytes, 42164224+12816384)
	}
	if row.UtilizationPercent != 13 || row.TemperatureCelsius != 67 {
		t.Errorf("dynamic fields did not join the row: %+v", row)
	}
	if row.Kind != "" {
		t.Errorf("kind = %q, want empty", row.Kind)
	}
}

// TestLiveAMDGPUsOnThisHost runs against the real /sys/class/drm. It is gated
// on NVPAIR_LIVE_AMD=1 because it can only pass on a host with an AMD GPU;
// the whole point is that the fake trees above cannot prove the real sysfs
// layout, only our reading of it.
func TestLiveAMDGPUsOnThisHost(t *testing.T) {
	if os.Getenv("NVPAIR_LIVE_AMD") != "1" {
		t.Skip("set NVPAIR_LIVE_AMD=1 on a host with an AMD GPU to run this")
	}

	gpus := detectAMDGPUs(drmClassDir)
	if len(gpus) == 0 {
		t.Fatalf("no AMD GPU found under %s", drmClassDir)
	}
	sampled := map[string]gpuStat{}
	if !decodeAMD(drmClassDir, sampled) {
		t.Fatal("decodeAMD reported no sample on a live AMD host; the node would report stale telemetry")
	}

	for _, gpu := range gpus {
		t.Logf("row: name=%q vram_bytes=%d statsKey=%q", gpu.Name, gpu.VramBytes, gpu.statsKey)
		if !strings.HasPrefix(gpu.Name, "AMD Radeon") {
			t.Errorf("Name = %q, want an AMD Radeon marketing name", gpu.Name)
		}
		if gpu.VramBytes <= 1<<30 {
			t.Errorf("VramBytes = %d, want more than 1 GiB", gpu.VramBytes)
		}
		if !strings.HasPrefix(gpu.statsKey, amdStatsKeyPrefix) {
			t.Errorf("statsKey = %q, want an %q key", gpu.statsKey, amdStatsKeyPrefix)
		}
		stat, ok := sampled[gpu.statsKey]
		if !ok {
			t.Fatalf("no live sample under %q; got %v", gpu.statsKey, sampled)
		}
		t.Logf("sample: util=%d%% used=%d bytes temp=%d C", stat.UtilizationPct, stat.VRAMUsed, stat.TemperatureC)
		if stat.TemperatureC == 0 {
			t.Errorf("TemperatureC = 0, want a real edge reading")
		}
		if stat.VRAMUsed == 0 {
			t.Errorf("VRAMUsed = 0, want the driver's usage figure")
		}
		if stat.VRAMUsed > gpu.VramBytes {
			t.Errorf("VRAMUsed %d exceeds capacity %d", stat.VRAMUsed, gpu.VramBytes)
		}
	}
}

// TestLiveCPUTemperatureOnThisHost confirms that the existing hwmon CPU
// package path (cputemp_linux.go) resolves on an AMD host, where the sensor is
// k10temp's Tctl rather than coretemp's "Package id 0". Gated with the GPU
// live test because it can only pass on a host that exposes one.
func TestLiveCPUTemperatureOnThisHost(t *testing.T) {
	if os.Getenv("NVPAIR_LIVE_AMD") != "1" {
		t.Skip("set NVPAIR_LIVE_AMD=1 on a host with an AMD CPU to run this")
	}

	source := findCPUTempSource()
	if source.path == "" {
		t.Fatal("no CPU package temperature source found")
	}
	t.Logf("cpu temperature source: %s", source.path)
	temp, ok := source.read()
	if !ok || temp == 0 {
		t.Fatalf("cpu temperature = %d, %v; want a real reading", temp, ok)
	}
	t.Logf("cpu temperature: %d C", temp)
}
