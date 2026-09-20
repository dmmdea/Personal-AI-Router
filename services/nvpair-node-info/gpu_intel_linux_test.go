// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The Intel inventory is a pure sysfs reader, so every case below runs against
// a fake tree under t.TempDir() shaped exactly like the real class:
// <root>/drm holds the card<N> entries whose `device` is a symlink into
// <root>/pci/<address>, and whose `driver` is a symlink into
// <root>/bus/pci/drivers/<name>. Both links are real, because resolving them
// is what the code under test does - the PCI address becomes the statsKey and
// the driver name is half the vendor filter.
//
// The values are the ones measured on the target host: an NVIDIA A2 on card0
// and an Intel CoffeeLake-S GT2 (8086:3e98, driver i915, no hwmon, no
// mem_info_vram_total) on card1.

// intelFakeTree is a sysfs-shaped temp tree for the DRM class.
type intelFakeTree struct {
	t          *testing.T
	drmRoot    string
	pciRoot    string
	driverRoot string
}

func newIntelFakeTree(t *testing.T) *intelFakeTree {
	t.Helper()
	root := t.TempDir()
	f := &intelFakeTree{
		t:          t,
		drmRoot:    filepath.Join(root, "drm"),
		pciRoot:    filepath.Join(root, "pci"),
		driverRoot: filepath.Join(root, "bus", "pci", "drivers"),
	}
	mkdir(t, f.drmRoot)
	mkdir(t, f.pciRoot)
	mkdir(t, f.driverRoot)
	return f
}

// addCard creates <drm>/<card> whose `device` symlinks to <pci>/<pciAddr>,
// populated with attrs, and whose `driver` symlinks to the shared
// <bus/pci/drivers>/<driver> directory - the same indirection real sysfs uses.
// An empty driver leaves the link out, standing in for an unbound device.
func (f *intelFakeTree) addCard(card, pciAddr, driver string, attrs map[string]string) string {
	f.t.Helper()
	deviceDir := filepath.Join(f.pciRoot, pciAddr)
	mkdir(f.t, deviceDir)
	writeAttrs(f.t, deviceDir, attrs)
	if driver != "" {
		driverDir := filepath.Join(f.driverRoot, driver)
		mkdir(f.t, driverDir)
		if err := os.Symlink(driverDir, filepath.Join(deviceDir, "driver")); err != nil {
			f.t.Fatalf("symlink %s driver: %v", card, err)
		}
	}
	cardDir := filepath.Join(f.drmRoot, card)
	mkdir(f.t, cardDir)
	if err := os.Symlink(deviceDir, filepath.Join(cardDir, "device")); err != nil {
		f.t.Fatalf("symlink %s device: %v", card, err)
	}
	return deviceDir
}

// addEntry creates a non-card class entry (a connector or render node) that
// carries an Intel vendor id, which is how the real class looks - an Intel
// iGPU normally owns every display connector on the box.
func (f *intelFakeTree) addEntry(name string, attrs map[string]string) {
	f.t.Helper()
	deviceDir := filepath.Join(f.drmRoot, name, "device")
	mkdir(f.t, deviceDir)
	writeAttrs(f.t, deviceDir, attrs)
}

// addHwmon creates <deviceDir>/hwmon/<hwmon> with the given files.
func (f *intelFakeTree) addHwmon(deviceDir, hwmon string, files map[string]string) {
	f.t.Helper()
	dir := filepath.Join(deviceDir, "hwmon", hwmon)
	mkdir(f.t, dir)
	writeAttrs(f.t, dir, files)
}

// igpuAttrs is the measured UHD Graphics 630 attribute set, with overrides
// applied. Note what is NOT here, because it is not there on the real host
// either: no mem_info_vram_total, no busy counter, no hwmon.
func igpuAttrs(overrides map[string]string) map[string]string {
	attrs := map[string]string{
		"vendor": "0x8086",
		"device": "0x3e98",
		"uevent": strings.Join([]string{
			"DRIVER=i915",
			"PCI_CLASS=30000",
			"PCI_ID=8086:3E98",
			"PCI_SLOT_NAME=0000:00:02.0",
		}, "\n"),
	}
	for k, v := range overrides {
		attrs[k] = v
	}
	return attrs
}

// TestDetectIntelGPUsIntegratedRow is the measured case: the iGPU that used to
// be invisible now produces a row, named, keyed, and marked unified so its
// capacity and usage come from system RAM the way the Mali rows do.
func TestDetectIntelGPUsIntegratedRow(t *testing.T) {
	f := newIntelFakeTree(t)
	f.addCard("card1", "0000:00:02.0", "i915", igpuAttrs(nil))

	gpus := detectIntelGPUs(f.drmRoot)
	if len(gpus) != 1 {
		t.Fatalf("detectIntelGPUs = %d rows, want 1: %+v", len(gpus), gpus)
	}
	got := gpus[0]
	if want := "Intel UHD Graphics 630 (Coffee Lake, Gen 9.5)"; got.Name != want {
		t.Errorf("Name = %q, want %q", got.Name, want)
	}
	if want := "intel:0000:00:02.0"; got.statsKey != want {
		t.Errorf("statsKey = %q, want %q", got.statsKey, want)
	}
	if !got.usesSystemMemoryUsage {
		t.Error("usesSystemMemoryUsage not set: an integrated GPU has no dedicated VRAM, its pool is system RAM")
	}
	if got.Kind != "" {
		t.Errorf("Kind = %q, want empty (an iGPU is a GPU, not an accelerator)", got.Kind)
	}
	// The ceiling, asserted rather than assumed: i915 publishes no busy
	// percentage in sysfs, so the row must carry none. A fabricated 0 would be
	// indistinguishable from a genuinely idle GPU.
	if got.UtilizationPercent != 0 {
		t.Errorf("UtilizationPercent = %d, want 0/absent: i915 exposes no sysfs busy counter", got.UtilizationPercent)
	}
	if got.TemperatureCelsius != 0 {
		t.Errorf("TemperatureCelsius = %d, want 0/absent: an iGPU has no sensor of its own", got.TemperatureCelsius)
	}
}

// TestDetectIntelGPUsSkipsNonIntelVendors keeps this path off cards that
// already have an owner: an NVIDIA card is nvidia-smi's, an AMD one is
// amdgpu's, and both have real telemetry this detector cannot match.
func TestDetectIntelGPUsSkipsNonIntelVendors(t *testing.T) {
	f := newIntelFakeTree(t)
	f.addCard("card0", "0000:01:00.0", "nvidia", map[string]string{
		"vendor": "0x10de", "device": "0x25b6",
	})
	f.addCard("card2", "0000:04:00.0", "amdgpu", map[string]string{
		"vendor": "0x1002", "device": "0x15e7",
	})

	if gpus := detectIntelGPUs(f.drmRoot); len(gpus) != 0 {
		t.Fatalf("detectIntelGPUs = %+v, want none", gpus)
	}
}

// TestDetectIntelGPUsRequiresAGraphicsDriver: the vendor id alone is not
// enough. Intel ships plenty of PCI devices, and a GPU bound to vfio-pci has
// been handed to a guest - this host cannot read its attributes and must not
// claim it.
func TestDetectIntelGPUsRequiresAGraphicsDriver(t *testing.T) {
	for _, driver := range []string{"vfio-pci", "", "xe_unknown"} {
		t.Run("driver="+driver, func(t *testing.T) {
			f := newIntelFakeTree(t)
			f.addCard("card1", "0000:00:02.0", driver, map[string]string{
				"vendor": "0x8086", "device": "0x3e98",
			})
			if gpus := detectIntelGPUs(f.drmRoot); len(gpus) != 0 {
				t.Fatalf("detectIntelGPUs = %+v, want none for driver %q", gpus, driver)
			}
		})
	}
}

// TestDetectIntelGPUsAcceptsBothDrivers: i915 owns everything up to Meteor
// Lake and the Arc A-series, xe owns Lunar Lake and Battlemage. Both are this
// host's GPU.
func TestDetectIntelGPUsAcceptsBothDrivers(t *testing.T) {
	for driver, device := range map[string]string{"i915": "0x3e98", "xe": "0x64a0"} {
		t.Run(driver, func(t *testing.T) {
			f := newIntelFakeTree(t)
			f.addCard("card0", "0000:00:02.0", driver, map[string]string{
				"vendor": "0x8086", "device": device,
			})
			if gpus := detectIntelGPUs(f.drmRoot); len(gpus) != 1 {
				t.Fatalf("detectIntelGPUs = %d rows, want 1 for driver %q", len(gpus), driver)
			}
		})
	}
}

// TestDRMDriverNameResolvesTheSymlink pins the resolution itself: sysfs points
// `driver` at a shared directory under /sys/bus/pci/drivers, so the name is
// the link target's base, not the link's own name.
func TestDRMDriverNameResolvesTheSymlink(t *testing.T) {
	f := newIntelFakeTree(t)
	dev := f.addCard("card1", "0000:00:02.0", "i915", igpuAttrs(nil))
	if got, want := drmDriverName(dev), "i915"; got != want {
		t.Errorf("drmDriverName = %q, want %q", got, want)
	}
}

// TestDRMDriverNameFallsBackToUevent covers a sysfs view where the link cannot
// be resolved (a container bind-mount, a copied tree). uevent's DRIVER= key
// carries the same string, so the card is still identified instead of dropped.
func TestDRMDriverNameFallsBackToUevent(t *testing.T) {
	f := newIntelFakeTree(t)
	deviceDir := filepath.Join(f.drmRoot, "card1", "device")
	mkdir(t, deviceDir)
	writeAttrs(t, deviceDir, igpuAttrs(nil))

	if got, want := drmDriverName(deviceDir), "i915"; got != want {
		t.Errorf("drmDriverName = %q, want %q", got, want)
	}
	gpus := detectIntelGPUs(f.drmRoot)
	if len(gpus) != 1 {
		t.Fatalf("detectIntelGPUs = %d rows, want 1 (uevent DRIVER= must be enough)", len(gpus))
	}
	// No resolvable device link either, so the key comes from PCI_SLOT_NAME.
	if got, want := gpus[0].statsKey, "intel:0000:00:02.0"; got != want {
		t.Errorf("statsKey = %q, want %q", got, want)
	}
}

// TestIntelModelName pins every table entry and both fallbacks. Each id was
// checked against the kernel's include/drm/intel/pciids.h and pci.ids rather
// than recalled - notably 0x9a60/0x9a68/0x9a70, which are Tiger Lake GT1 and
// sold as UHD Graphics, not as Iris Xe.
func TestIntelModelName(t *testing.T) {
	cases := []struct{ id, want string }{
		{"3e91", "Intel UHD Graphics 630 (Coffee Lake, Gen 9.5)"},
		{"3e92", "Intel UHD Graphics 630 (Coffee Lake, Gen 9.5)"},
		{"3e98", "Intel UHD Graphics 630 (Coffee Lake, Gen 9.5)"},
		{"3e9b", "Intel UHD Graphics 630 (Coffee Lake, Gen 9.5)"},
		{"9a60", "Intel UHD Graphics (Tiger Lake, Xe-LP)"},
		{"9a68", "Intel UHD Graphics (Tiger Lake, Xe-LP)"},
		{"9a70", "Intel UHD Graphics (Tiger Lake, Xe-LP)"},
		{"9a40", "Intel Iris Xe Graphics (Tiger Lake, Xe-LP)"},
		{"9a49", "Intel Iris Xe Graphics (Tiger Lake, Xe-LP)"},
		{"46a6", "Intel Iris Xe Graphics (Alder Lake, Xe-LP)"},
		{"46a8", "Intel Iris Xe Graphics (Alder Lake, Xe-LP)"},
		{"46aa", "Intel Iris Xe Graphics (Alder Lake, Xe-LP)"},
		{"a7a0", "Intel Iris Xe Graphics (Raptor Lake, Xe-LP)"},
		{"a7a1", "Intel Iris Xe Graphics (Raptor Lake, Xe-LP)"},
		{"56a0", "Intel Arc A770 (Alchemist, Xe-HPG)"},
		{"56a1", "Intel Arc A750 (Alchemist, Xe-HPG)"},
		{"7d55", "Intel Arc Graphics (Meteor Lake, Xe-LPG)"},
		{"7dd5", "Intel Graphics (Meteor Lake, Xe-LPG)"},
		{"64a0", "Intel Arc Graphics 130V/140V (Lunar Lake, Xe2)"},
		{"e20b", "Intel Arc B580 (Battlemage, Xe2-HPG)"},
		{"abcd", "Intel Graphics (device 0xabcd)"},
		{"", "Intel Graphics"},
	}
	for _, c := range cases {
		if got := intelModelName(c.id); got != c.want {
			t.Errorf("intelModelName(%q) = %q, want %q", c.id, got, c.want)
		}
	}
}

// TestIntelModelNameNamesTheGeneration mirrors the AMD rule: a node list has
// to say which graphics generation the part is, because that is what decides
// what it can run. A bare marketing name does not.
func TestIntelModelNameNamesTheGeneration(t *testing.T) {
	architectures := []string{"Gen 9.5", "Xe-LP", "Xe-HPG", "Xe-LPG", "Xe2", "Xe2-HPG"}
	for id := range intelModels {
		name := intelModelName(id)
		named := false
		for _, arch := range architectures {
			if strings.HasSuffix(name, ", "+arch+")") {
				named = true
				break
			}
		}
		if !named {
			t.Errorf("intelModels[%q] = %q; every name must end in a known architecture %v", id, name, architectures)
		}
		if !strings.HasPrefix(name, "Intel ") {
			t.Errorf("intelModels[%q] = %q; want a vendor-prefixed name", id, name)
		}
	}
}

// TestDetectIntelGPUsUnlistedIDKeepsTheRow: a part released after intelModels
// was written is still published, under a name that carries its device id, so
// it is never silently missing from a node's inventory.
func TestDetectIntelGPUsUnlistedIDKeepsTheRow(t *testing.T) {
	f := newIntelFakeTree(t)
	f.addCard("card0", "0000:00:02.0", "xe", map[string]string{
		"vendor": "0x8086", "device": "0xffff",
	})

	gpus := detectIntelGPUs(f.drmRoot)
	if len(gpus) != 1 {
		t.Fatalf("detectIntelGPUs = %d rows, want 1", len(gpus))
	}
	if got, want := gpus[0].Name, "Intel Graphics (device 0xffff)"; got != want {
		t.Errorf("Name = %q, want %q", got, want)
	}
	if !gpus[0].usesSystemMemoryUsage {
		t.Error("an unlisted card with no mem_info_vram_total must be judged integrated by its own numbers")
	}
}

// TestDetectIntelGPUsDiscreteReadsVRAM: an Arc card has its own memory, so its
// capacity is the driver's figure and it is NOT marked unified - substituting
// system RAM there would misreport a 12 GiB card as the size of the host.
func TestDetectIntelGPUsDiscreteReadsVRAM(t *testing.T) {
	f := newIntelFakeTree(t)
	f.addCard("card0", "0000:03:00.0", "xe", map[string]string{
		"vendor": "0x8086", "device": "0xe20b",
		"mem_info_vram_total": "12884901888",
	})

	gpus := detectIntelGPUs(f.drmRoot)
	if len(gpus) != 1 {
		t.Fatalf("detectIntelGPUs = %d rows, want 1", len(gpus))
	}
	if got, want := gpus[0].VramBytes, uint64(12884901888); got != want {
		t.Errorf("VramBytes = %d, want %d", got, want)
	}
	if gpus[0].usesSystemMemoryUsage {
		t.Error("usesSystemMemoryUsage set on a discrete card: its VRAM is not system RAM")
	}
}

// TestDetectIntelGPUsDiscreteWithoutVRAMAttributeLeavesCapacityUnknown: i915
// does not publish mem_info_vram_total on every kernel, and 0 (omitted from
// the wire) is the honest answer. An invented capacity is worse than none.
func TestDetectIntelGPUsDiscreteWithoutVRAMAttributeLeavesCapacityUnknown(t *testing.T) {
	f := newIntelFakeTree(t)
	f.addCard("card0", "0000:03:00.0", "i915", map[string]string{
		"vendor": "0x8086", "device": "0x56a0",
	})

	gpus := detectIntelGPUs(f.drmRoot)
	if len(gpus) != 1 {
		t.Fatalf("detectIntelGPUs = %d rows, want 1", len(gpus))
	}
	if gpus[0].VramBytes != 0 {
		t.Errorf("VramBytes = %d, want 0 (unknown)", gpus[0].VramBytes)
	}
	if gpus[0].usesSystemMemoryUsage {
		t.Error("a listed discrete card must not be reported as unified memory")
	}
}

// TestDetectIntelGPUsIgnoresConnectorDirs is the duplicate-row guard. It
// matters more for Intel than for AMD: the iGPU is usually the card that owns
// every display output, so the real class carries several of these.
func TestDetectIntelGPUsIgnoresConnectorDirs(t *testing.T) {
	f := newIntelFakeTree(t)
	f.addCard("card1", "0000:00:02.0", "i915", igpuAttrs(nil))
	for _, name := range []string{"card1-DP-1", "card1-DP-2", "card1-HDMI-A-1", "renderD129", "version"} {
		f.addEntry(name, map[string]string{"vendor": "0x8086", "device": "0x3e98"})
	}

	gpus := detectIntelGPUs(f.drmRoot)
	if len(gpus) != 1 {
		t.Fatalf("detectIntelGPUs = %d rows, want 1 (connectors must not list the GPU again): %+v", len(gpus), gpus)
	}
}

// TestDetectIntelGPUsOrdersByCardNumber pins a stable inventory order across
// boots; lexical order would put card10 before card2.
func TestDetectIntelGPUsOrdersByCardNumber(t *testing.T) {
	f := newIntelFakeTree(t)
	f.addCard("card10", "0000:0a:00.0", "xe", map[string]string{"vendor": "0x8086", "device": "0xe20b"})
	f.addCard("card2", "0000:00:02.0", "i915", map[string]string{"vendor": "0x8086", "device": "0x3e98"})

	gpus := detectIntelGPUs(f.drmRoot)
	if len(gpus) != 2 {
		t.Fatalf("detectIntelGPUs = %d rows, want 2", len(gpus))
	}
	if gpus[0].statsKey != "intel:0000:00:02.0" || gpus[1].statsKey != "intel:0000:0a:00.0" {
		t.Fatalf("order = %q, %q; want card2 then card10", gpus[0].statsKey, gpus[1].statsKey)
	}
}

// TestIntelTemperaturesArcOnly: an Arc card registers a hwmon, an iGPU does
// not. The iGPU must contribute nothing at all rather than borrowing the CPU
// package sensor, which measures a different piece of silicon.
func TestIntelTemperaturesArcOnly(t *testing.T) {
	f := newIntelFakeTree(t)
	f.addCard("card1", "0000:00:02.0", "i915", igpuAttrs(nil))
	arc := f.addCard("card0", "0000:03:00.0", "xe", map[string]string{
		"vendor": "0x8086", "device": "0xe20b", "mem_info_vram_total": "12884901888",
	})
	f.addHwmon(arc, "hwmon3", map[string]string{"name": "xe", "temp1_input": "54000"})

	temps := intelTemperatures(f.drmRoot)
	if len(temps) != 1 {
		t.Fatalf("intelTemperatures = %v, want exactly the Arc card", temps)
	}
	if got, want := temps["intel:0000:03:00.0"], uint32(54); got != want {
		t.Errorf("Arc temperature = %d, want %d", got, want)
	}
	if _, ok := temps["intel:0000:00:02.0"]; ok {
		t.Error("the integrated GPU reported a temperature; it has no sensor of its own")
	}
}

// TestMergeIntelStatsLeavesThePublishedMapAloneWhenThereIsNothingToAdd is the
// aliasing guard and the cost check in one. On the common host - no Intel
// card, or an iGPU with no sensor - the snapshot's map must be left exactly as
// it was, because on a stale-preserve tick it aliases the previous,
// already-published map that HTTP handlers are reading right now.
func TestMergeIntelStatsLeavesThePublishedMapAloneWhenThereIsNothingToAdd(t *testing.T) {
	f := newIntelFakeTree(t)
	f.addCard("card1", "0000:00:02.0", "i915", igpuAttrs(nil)) // no hwmon, as measured

	published := map[string]gpuStat{"GPU-1234": {UtilizationPct: 42}}
	snap := &statsSnapshot{GPU: published}
	mergeIntelStatsFrom(f.drmRoot, snap)

	if len(published) != 1 {
		t.Fatalf("the previously published map was mutated: %+v", published)
	}
	if len(snap.GPU) != 1 || snap.GPU["GPU-1234"].UtilizationPct != 42 {
		t.Fatalf("snapshot GPU map changed: %+v", snap.GPU)
	}
	if !snap.GPUSampledAt.IsZero() {
		t.Error("GPUSampledAt advanced: an Intel row is never fresh GPU telemetry")
	}
}

// TestMergeIntelStatsClonesBeforeAddingAnArcTemperature: when there IS
// something to add, the previously published map must not be the one that
// grows - a handler mid-read would otherwise see a map mutate underneath it.
func TestMergeIntelStatsClonesBeforeAddingAnArcTemperature(t *testing.T) {
	f := newIntelFakeTree(t)
	arc := f.addCard("card0", "0000:03:00.0", "xe", map[string]string{
		"vendor": "0x8086", "device": "0xe20b", "mem_info_vram_total": "12884901888",
	})
	f.addHwmon(arc, "hwmon3", map[string]string{"name": "xe", "temp1_input": "54000"})

	published := map[string]gpuStat{"GPU-1234": {UtilizationPct: 42}}
	snap := &statsSnapshot{GPU: published}
	mergeIntelStatsFrom(f.drmRoot, snap)

	if len(published) != 1 {
		t.Fatalf("the previously published map was mutated: %+v", published)
	}
	if got, want := snap.GPU["intel:0000:03:00.0"].TemperatureC, uint32(54); got != want {
		t.Errorf("Arc TemperatureC = %d, want %d", got, want)
	}
	if got := snap.GPU["GPU-1234"].UtilizationPct; got != 42 {
		t.Errorf("existing row lost: UtilizationPct = %d, want 42", got)
	}
	if !snap.GPUSampledAt.IsZero() {
		t.Error("GPUSampledAt advanced: a temperature is not a utilization sample")
	}
}

// TestComposeLinuxGPUsAppendsEveryVendor is the fix for the defect this work
// started from: a node with a discrete card and an integrated one published
// only the discrete card, because the detector chain returned at the first
// source that produced anything. Every vendor's rows must now appear, in a
// fixed order, with the NVIDIA card still first.
func TestComposeLinuxGPUsAppendsEveryVendor(t *testing.T) {
	ghwCalled := false
	ghw := func() []GPUInfo {
		ghwCalled = true
		return []GPUInfo{{Name: "ghw fallback"}}
	}
	got := composeLinuxGPUs(
		[]GPUInfo{{Name: "NVIDIA A2", statsKey: "GPU-1234"}},
		[]GPUInfo{{Name: "Arm Mali-G610 MP4", statsKey: "mali:gpu"}},
		[]GPUInfo{{Name: "AMD Radeon Vega Graphics (Barcelo, GCN 5.1)", statsKey: "amd:0000:04:00.0"}},
		[]GPUInfo{{Name: "Intel UHD Graphics 630 (Coffee Lake, Gen 9.5)", statsKey: "intel:0000:00:02.0"}},
		ghw,
	)
	want := []string{
		"NVIDIA A2",
		"Arm Mali-G610 MP4",
		"AMD Radeon Vega Graphics (Barcelo, GCN 5.1)",
		"Intel UHD Graphics 630 (Coffee Lake, Gen 9.5)",
	}
	if len(got) != len(want) {
		t.Fatalf("composeLinuxGPUs = %d rows, want %d: %+v", len(got), len(want), got)
	}
	for i, name := range want {
		if got[i].Name != name {
			t.Errorf("row %d = %q, want %q", i, got[i].Name, name)
		}
	}
	if ghwCalled {
		t.Error("ghw ran alongside the vendor detectors; it would list the same adapters again under worse names")
	}
}

// TestComposeLinuxGPUsFallsBackToGHWOnlyWhenEmpty keeps the last resort last:
// a host no vendor detector recognizes still lists its adapters by name.
func TestComposeLinuxGPUsFallsBackToGHWOnlyWhenEmpty(t *testing.T) {
	got := composeLinuxGPUs(nil, nil, nil, nil, func() []GPUInfo {
		return []GPUInfo{{Name: "ghw fallback"}}
	})
	if len(got) != 1 || got[0].Name != "ghw fallback" {
		t.Fatalf("composeLinuxGPUs = %+v, want the ghw fallback", got)
	}
}
