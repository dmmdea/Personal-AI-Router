// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"
	"unsafe"

	"nvpair-shared/noderec"
)

// TestHailoChipTemperatureSize pins the in-Go layout of
// hailo_chip_temperature_info_t: two float32 sensors + uint16 sample count,
// padded to the struct's 4-byte alignment. hailoDevice.temperature hands a
// pointer to one of these straight to the library, so a layout drift would
// have the firmware write past it or have us read the sample count out of a
// temperature's bits.
func TestHailoChipTemperatureSize(t *testing.T) {
	const expected = 12
	if got := unsafe.Sizeof(hailoChipTemperature{}); got != expected {
		t.Fatalf("hailoChipTemperature size = %d, want %d", got, expected)
	}
	var info hailoChipTemperature
	if got := unsafe.Offsetof(info.TS1); got != 4 {
		t.Errorf("TS1 offset = %d, want 4", got)
	}
	if got := unsafe.Offsetof(info.SampleCount); got != 8 {
		t.Errorf("SampleCount offset = %d, want 8", got)
	}
}

// TestHailoDeviceIdentityLayout pins hailo_device_identity_t against the C
// header's field order, including the padding byte the compiler inserts
// before the 4-byte device_architecture enum. If this fires,
// hailoDevice.architecture is reading some other field's bytes and would name
// every accelerator from garbage.
//
//	  0  protocol_version                 uint32
//	  4  fw_version                       3 x uint32
//	 16  logger_version                   uint32
//	 20  board_name_length                uint8
//	 21  board_name[32]
//	 53  is_release                       bool
//	 54  extended_context_switch_buffer   bool
//	 55  (alignment padding)
//	 56  device_architecture              enum (int32)
//	 60  serial_number_length             uint8
//	 61  serial_number[16]
//	 77  part_number_length               uint8
//	 78  part_number[16]
//	 94  product_name_length              uint8
//	 95  product_name[42]
//	---
//	140 bytes (rounded to the struct's 4-byte alignment)
func TestHailoDeviceIdentityLayout(t *testing.T) {
	var id hailoDeviceIdentity
	if got := unsafe.Sizeof(id); got != 140 {
		t.Fatalf("hailoDeviceIdentity size = %d, want 140", got)
	}
	offsets := map[string]uintptr{
		"FWVersion":          unsafe.Offsetof(id.FWVersion),
		"LoggerVersion":      unsafe.Offsetof(id.LoggerVersion),
		"BoardName":          unsafe.Offsetof(id.BoardName),
		"IsRelease":          unsafe.Offsetof(id.IsRelease),
		"DeviceArchitecture": unsafe.Offsetof(id.DeviceArchitecture),
		"SerialNumber":       unsafe.Offsetof(id.SerialNumber),
		"PartNumber":         unsafe.Offsetof(id.PartNumber),
		"ProductName":        unsafe.Offsetof(id.ProductName),
	}
	want := map[string]uintptr{
		"FWVersion":          4,
		"LoggerVersion":      16,
		"BoardName":          21,
		"IsRelease":          53,
		"DeviceArchitecture": 56,
		"SerialNumber":       61,
		"PartNumber":         78,
		"ProductName":        95,
	}
	if !reflect.DeepEqual(offsets, want) {
		t.Fatalf("hailoDeviceIdentity offsets = %v, want %v", offsets, want)
	}
	if got := unsafe.Sizeof(hailoDeviceID{}); got != 32 {
		t.Errorf("hailoDeviceID size = %d, want 32 (HAILO_MAX_DEVICE_ID_LENGTH)", got)
	}
}

// TestHailoDeviceIdentityPlausible covers the guard that keeps a struct we
// misread from naming a device: every length field must fit its array and the
// architecture must be one the header defines.
func TestHailoDeviceIdentityPlausible(t *testing.T) {
	good := hailoDeviceIdentity{
		BoardNameLength:    10,
		SerialNumberLength: 12,
		PartNumberLength:   9,
		ProductNameLength:  42,
		DeviceArchitecture: hailoArchHailo8L,
	}
	if !good.plausible() {
		t.Fatal("a well-formed identity was rejected")
	}
	cases := map[string]func(*hailoDeviceIdentity){
		"board name longer than its array":   func(i *hailoDeviceIdentity) { i.BoardNameLength = 33 },
		"serial longer than its array":       func(i *hailoDeviceIdentity) { i.SerialNumberLength = 17 },
		"part number longer than its array":  func(i *hailoDeviceIdentity) { i.PartNumberLength = 17 },
		"product name longer than its array": func(i *hailoDeviceIdentity) { i.ProductNameLength = 43 },
		"architecture out of range":          func(i *hailoDeviceIdentity) { i.DeviceArchitecture = hailoArchCount },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			bad := good
			mutate(&bad)
			if bad.plausible() {
				t.Fatal("an implausible identity passed the guard")
			}
		})
	}
}

// TestHailoArchName pins the architecture-to-name table. The live row's name
// is what a user reads in the panel, and "Hailo-8" vs "Hailo-8L" is the
// difference between a 26 and a 13 TOPS part.
func TestHailoArchName(t *testing.T) {
	cases := map[uint32]string{
		hailoArchHailo8A0: "Hailo-8 AI Accelerator",
		hailoArchHailo8:   "Hailo-8 AI Accelerator",
		hailoArchHailo8L:  "Hailo-8L AI Accelerator",
		hailoArchHailo15H: "Hailo-15H AI Accelerator",
		hailoArchHailo15L: "Hailo-15L AI Accelerator",
		hailoArchHailo15M: "Hailo-15M AI Accelerator",
		hailoArchHailo10H: "Hailo-10H AI Accelerator",
		hailoArchMars:     hailoGenericName,
		999:               hailoGenericName,
	}
	for arch, want := range cases {
		if got := hailoArchName(arch); got != want {
			t.Errorf("hailoArchName(%d) = %q, want %q", arch, got, want)
		}
	}
}

// TestHailoPnPDeviceID covers the PnP enumerator key parse that drives the
// presence fallback: only Hailo's vendor id counts, the device id is hex, and
// a foreign or malformed key is rejected rather than listed as an accelerator.
func TestHailoPnPDeviceID(t *testing.T) {
	cases := []struct {
		key    string
		want   uint16
		wantOK bool
	}{
		{"VEN_1E60&DEV_2864&SUBSYS_28641E60&REV_01", 0x2864, true},
		{"ven_1e60&dev_2864&subsys_28641e60&rev_01", 0x2864, true},
		{"VEN_1E60&DEV_45C4&SUBSYS_45C41E60&REV_01", 0x45c4, true},
		{"VEN_1E60&DEV_2864", 0x2864, true},
		{"VEN_10DE&DEV_2864&SUBSYS_00000000&REV_A1", 0, false}, // a different vendor
		{"VEN_1E60&SUBSYS_28641E60&REV_01", 0, false},          // no device id
		{"VEN_1E60&DEV_ZZZZ", 0, false},                        // not hex
		{"", 0, false},
	}
	for _, tc := range cases {
		got, ok := hailoPnPDeviceID(tc.key)
		if ok != tc.wantOK || got != tc.want {
			t.Errorf("hailoPnPDeviceID(%q) = (%#x, %v), want (%#x, %v)", tc.key, got, ok, tc.want, tc.wantOK)
		}
	}
}

func TestHailoPCIDeviceName(t *testing.T) {
	if got := hailoPCIDeviceName(0x2864); got != "Hailo-8L AI Accelerator" {
		t.Errorf("hailoPCIDeviceName(0x2864) = %q", got)
	}
	if got := hailoPCIDeviceName(0xbeef); got != hailoGenericName {
		t.Errorf("hailoPCIDeviceName(unknown) = %q, want %q", got, hailoGenericName)
	}
}

// fakeRegistry answers subkey listings from a map, so the presence scan can be
// exercised without a Hailo module or a live registry.
func fakeRegistry(keys map[string][]string) subKeyLister {
	return func(path string) ([]string, error) {
		v, ok := keys[path]
		if !ok {
			return nil, fmt.Errorf("open %s: key not found", path)
		}
		return v, nil
	}
}

// TestHailoPresenceAccelerators covers the fallback that lists a fitted module
// on a host without HailoRT: one row per PnP instance, named from the device
// id, with no temperature and a statsKey no sampler can ever collide with.
func TestHailoPresenceAccelerators(t *testing.T) {
	list := fakeRegistry(map[string][]string{
		pciEnumKey: {
			"VEN_8086&DEV_A0A3&SUBSYS_00000000&REV_20",
			"VEN_1E60&DEV_2864&SUBSYS_28641E60&REV_01",
			"VEN_10DE&DEV_2782&SUBSYS_88C41043&REV_A1",
		},
		pciEnumKey + `\VEN_1E60&DEV_2864&SUBSYS_28641E60&REV_01`: {"4&1d2b0e6e&0&00E5"},
	})
	rows := hailoPresenceAccelerators(list)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1: %+v", len(rows), rows)
	}
	row := rows[0]
	if row.Name != "Hailo-8L AI Accelerator" {
		t.Errorf("name = %q", row.Name)
	}
	if row.Kind != noderec.GPUKindAccelerator {
		t.Errorf("kind = %q, want %q", row.Kind, noderec.GPUKindAccelerator)
	}
	want := hailoStatsKey(`pnp:VEN_1E60&DEV_2864&SUBSYS_28641E60&REV_01\4&1d2b0e6e&0&00E5`)
	if row.statsKey != want {
		t.Errorf("statsKey = %q, want %q", row.statsKey, want)
	}
	if row.VramBytes != 0 || row.UtilizationPercent != 0 || row.TemperatureCelsius != 0 {
		t.Errorf("presence row carries dynamic fields: %+v", row)
	}
	if !row.UtilizationUnavailable || row.InferenceReady == nil || *row.InferenceReady {
		t.Errorf("presence row = %+v, want utilization_unavailable and inference_ready=false like every Hailo row", row)
	}
}

// TestHailoPresenceAcceleratorsTwoModules pins that each instance under a
// device key becomes its own row with its own key, and that an unreadable
// instance list still yields the device itself.
func TestHailoPresenceAcceleratorsTwoModules(t *testing.T) {
	list := fakeRegistry(map[string][]string{
		pciEnumKey: {
			"VEN_1E60&DEV_2864&SUBSYS_28641E60&REV_01",
			"VEN_1E60&DEV_45C4&SUBSYS_45C41E60&REV_01",
		},
		pciEnumKey + `\VEN_1E60&DEV_2864&SUBSYS_28641E60&REV_01`: {"inst_a", "inst_b"},
		// DEV_45C4's instance list is deliberately absent: unreadable.
	})
	rows := hailoPresenceAccelerators(list)
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3: %+v", len(rows), rows)
	}
	seen := map[string]struct{}{}
	for _, r := range rows {
		if _, dup := seen[r.statsKey]; dup {
			t.Fatalf("duplicate statsKey %q", r.statsKey)
		}
		seen[r.statsKey] = struct{}{}
	}
	if rows[2].Name != hailoGenericName {
		t.Errorf("unknown device id named %q, want %q", rows[2].Name, hailoGenericName)
	}
}

// TestHailoPresenceAcceleratorsNoRegistry pins the silent no-op when the PnP
// branch cannot be read at all.
func TestHailoPresenceAcceleratorsNoRegistry(t *testing.T) {
	if rows := hailoPresenceAccelerators(fakeRegistry(nil)); rows != nil {
		t.Fatalf("rows = %+v, want nil", rows)
	}
}

// TestHailoDLLCandidates pins the search order: an explicit environment
// override first (both spellings of where it points), then the default
// install location, then the bare name for the loader's own search.
func TestHailoDLLCandidates(t *testing.T) {
	env := func(vars map[string]string) func(string) string {
		return func(k string) string { return vars[k] }
	}

	got := hailoDLLCandidates(env(map[string]string{"ProgramFiles": `C:\Program Files`}))
	want := []string{
		`C:\Program Files\HailoRT\bin\libhailort.dll`,
		hailoDLLName,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("no-override candidates = %v, want %v", got, want)
	}

	got = hailoDLLCandidates(env(map[string]string{
		"HAILORT_DIR":  `D:\opt\hailort`,
		"ProgramFiles": `E:\Programs`,
	}))
	want = []string{
		`D:\opt\hailort\libhailort.dll`,
		`D:\opt\hailort\bin\libhailort.dll`,
		`E:\Programs\HailoRT\bin\libhailort.dll`,
		hailoDLLName,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("HAILORT_DIR candidates = %v, want %v", got, want)
	}

	got = hailoDLLCandidates(env(map[string]string{"HAILORT_ROOT": `D:\hailo`}))
	want = []string{
		`D:\hailo\libhailort.dll`,
		`D:\hailo\bin\libhailort.dll`,
		`C:\Program Files\HailoRT\bin\libhailort.dll`, // ProgramFiles unset: documented default
		hailoDLLName,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("HAILORT_ROOT candidates = %v, want %v", got, want)
	}

	// A variable pointing at the default install directory must not produce
	// the same path twice.
	got = hailoDLLCandidates(env(map[string]string{
		"HAILORT_DIR":  `C:\Program Files\HailoRT`,
		"ProgramFiles": `C:\Program Files`,
	}))
	want = []string{
		`C:\Program Files\HailoRT\libhailort.dll`,
		`C:\Program Files\HailoRT\bin\libhailort.dll`,
		hailoDLLName,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("deduped candidates = %v, want %v", got, want)
	}
}

func TestResolveHailoDLL(t *testing.T) {
	env := func(k string) string {
		switch k {
		case "HAILORT_DIR":
			return `D:\opt\hailort`
		case "ProgramFiles":
			return `C:\Program Files`
		}
		return ""
	}
	present := func(paths ...string) func(string) bool {
		set := map[string]struct{}{}
		for _, p := range paths {
			set[p] = struct{}{}
		}
		return func(p string) bool {
			_, ok := set[p]
			return ok
		}
	}

	// The override wins over an equally present default install.
	got := resolveHailoDLL(env, present(
		`D:\opt\hailort\bin\libhailort.dll`,
		`C:\Program Files\HailoRT\bin\libhailort.dll`,
	))
	if want := `D:\opt\hailort\bin\libhailort.dll`; got != want {
		t.Errorf("resolveHailoDLL = %q, want %q", got, want)
	}

	// Nothing under the override: the default install answers.
	got = resolveHailoDLL(env, present(`C:\Program Files\HailoRT\bin\libhailort.dll`))
	if want := `C:\Program Files\HailoRT\bin\libhailort.dll`; got != want {
		t.Errorf("resolveHailoDLL = %q, want %q", got, want)
	}

	// Nothing on disk: the bare name, so the loader's search path decides and
	// a genuinely missing runtime surfaces as one ordinary load error.
	if got := resolveHailoDLL(env, present()); got != hailoDLLName {
		t.Errorf("resolveHailoDLL = %q, want %q", got, hailoDLLName)
	}
}

// TestHailoTemperature pins the reported figure: the hotter of the two die
// sensors, rounded, and nothing at all when the firmware has taken no sample
// or the values are not a temperature. A published zero would render as
// "0 °C" through the omitempty contract's one ambiguity, so this path must
// report unavailable instead.
func TestHailoTemperature(t *testing.T) {
	cases := []struct {
		name     string
		ts0, ts1 float32
		samples  uint16
		want     uint32
		wantOK   bool
	}{
		{"ts0 hotter", 51.4, 48.9, 8, 51, true},
		{"ts1 hotter", 44.2, 49.6, 8, 50, true},
		{"rounds to nearest", 47.5, 47.5, 1, 48, true},
		{"single sample is enough", 39.2, 38.8, 1, 39, true},
		{"no samples yet", 51.4, 48.9, 0, 0, false},
		{"both sensors zero", 0, 0, 8, 0, false},
		{"negative reading", -40, -40, 8, 0, false},
		{"one sensor negative, one real", -40, 46.7, 8, 47, true},
		{"absurdly hot", 900, 900, 8, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := hailoTemperature(tc.ts0, tc.ts1, tc.samples)
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("hailoTemperature(%v, %v, %d) = (%d, %v), want (%d, %v)",
					tc.ts0, tc.ts1, tc.samples, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// TestHailoSamplerReopensAfterConsecutiveFailures drives the sampler's whole
// state machine with an injected reader: it publishes a reading, holds the
// last good value through transient failures, drops the handle only on the
// third consecutive one, and recovers on the reopened device. Without the
// reopen a driver restart would silently freeze the temperature forever.
func TestHailoSamplerReopensAfterConsecutiveFailures(t *testing.T) {
	var (
		opens    int
		releases int
		ts0, ts1 float32
		samples  uint16
		readErr  error
	)
	s := newHailoSampler("hailo:0000:03:00.0", "0000:03:00.0", func() (hailoTempReader, func(), error) {
		opens++
		return func() (float32, float32, uint16, error) {
			return ts0, ts1, samples, readErr
		}, func() { releases++ }, nil
	})

	if _, ok := s.Latest(); ok {
		t.Fatal("a fresh sampler published a sample")
	}

	ts0, ts1, samples = 51.4, 48.9, 8
	s.sample()
	if opens != 1 {
		t.Fatalf("opens = %d, want 1", opens)
	}
	st, ok := s.Latest()
	if !ok || st.TemperatureC != 51 {
		t.Fatalf("Latest() = (%+v, %v), want 51 C", st, ok)
	}
	if st.UtilizationPct != 0 || st.VRAMUsed != 0 {
		t.Fatalf("sampler published a fabricated utilization or VRAM figure: %+v", st)
	}

	// Two failures: the handle is kept and the last good reading stands.
	readErr = errors.New("HAILO_TIMEOUT")
	s.sample()
	s.sample()
	if opens != 1 || releases != 0 {
		t.Fatalf("after 2 failures opens=%d releases=%d, want 1 and 0", opens, releases)
	}
	if st, _ := s.Latest(); st.TemperatureC != 51 {
		t.Fatalf("a transient failure cleared the reading: %+v", st)
	}

	// The third consecutive failure drops the handle.
	s.sample()
	if releases != 1 {
		t.Fatalf("releases = %d, want 1 after %d consecutive failures", releases, hailoReopenAfterFailures)
	}
	if s.read != nil {
		t.Fatal("the failed handle was kept")
	}

	// The next sample reopens; the device answers again.
	readErr = nil
	ts0, ts1, samples = 55.2, 56.8, 4
	s.sample()
	if opens != 2 {
		t.Fatalf("opens = %d, want 2 (reopen)", opens)
	}
	if st, _ := s.Latest(); st.TemperatureC != 57 {
		t.Fatalf("Latest() after reopen = %+v, want 57 C", st)
	}

	// A single failure after recovery must not reopen again immediately: the
	// counter resets on every successful read.
	readErr = errors.New("HAILO_TIMEOUT")
	s.sample()
	if opens != 2 || releases != 1 {
		t.Fatalf("one failure after recovery opens=%d releases=%d, want 2 and 1", opens, releases)
	}
}

// TestHailoSamplerOpenFailureIsSilentAbsence pins the no-device path: a
// sampler whose device never opens publishes nothing at all rather than a
// zero temperature, and keeps retrying.
func TestHailoSamplerOpenFailureIsSilentAbsence(t *testing.T) {
	attempts := 0
	s := newHailoSampler("hailo:x", "x", func() (hailoTempReader, func(), error) {
		attempts++
		return nil, nil, errors.New("hailo_create_device_by_id: hailo_status 74")
	})
	s.sample()
	s.sample()
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2 (every tick retries)", attempts)
	}
	if _, ok := s.Latest(); ok {
		t.Fatal("an unopenable device published a sample")
	}
}

// TestHailoSamplerSampleCountZeroKeepsHandle pins that a successful call with
// no samples behind it is not a failure: it must not count toward the reopen
// rule, and it must not publish.
func TestHailoSamplerSampleCountZeroKeepsHandle(t *testing.T) {
	opens, releases := 0, 0
	s := newHailoSampler("hailo:x", "x", func() (hailoTempReader, func(), error) {
		opens++
		return func() (float32, float32, uint16, error) { return 0, 0, 0, nil }, func() { releases++ }, nil
	})
	for i := 0; i < hailoReopenAfterFailures+2; i++ {
		s.sample()
	}
	if opens != 1 || releases != 0 {
		t.Fatalf("opens=%d releases=%d, want 1 and 0", opens, releases)
	}
	if _, ok := s.Latest(); ok {
		t.Fatal("a sample-less reading was published")
	}
}

// TestHailoSamplerStopReleasesDevice pins shutdown: the goroutine exits and
// the handle is released exactly once.
func TestHailoSamplerStopReleasesDevice(t *testing.T) {
	released := make(chan struct{})
	s := newHailoSampler("hailo:x", "x", func() (hailoTempReader, func(), error) {
		return func() (float32, float32, uint16, error) { return 44, 45, 2, nil },
			func() { close(released) }, nil
	})
	go s.run()
	done := make(chan struct{})
	go func() { s.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return")
	}
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("Stop did not release the device handle")
	}
}

// TestHailoStatsKey pins the namespaced join key and its normalization: the
// inventory row and the sampler must derive the same string from the same
// device id, whatever case the library reports it in.
func TestHailoStatsKey(t *testing.T) {
	if got := hailoStatsKey("0000:03:00.0"); got != "hailo:0000:03:00.0" {
		t.Fatalf("hailoStatsKey = %q", got)
	}
	if hailoStatsKey(" 0000:03:00.0 ") != hailoStatsKey("0000:03:00.0") {
		t.Fatal("whitespace changed the join key")
	}
	if hailoStatsKey("0000:03:00.0") != hailoStatsKey("0000:03:00.0") {
		t.Fatal("hailoStatsKey is not deterministic")
	}
}

// TestHailoAcceleratorRowShape pins the inventory contract for a Hailo row:
// an npu kind that never contributes to node GPU pressure, no VRAM, and the
// sampler's join key.
func TestHailoAcceleratorRowShape(t *testing.T) {
	row := hailoRow(hailoArchName(hailoArchHailo8L), hailoStatsKey("0000:03:00.0"))
	if row.Name != "Hailo-8L AI Accelerator" || row.statsKey != "hailo:0000:03:00.0" || row.Kind != noderec.GPUKindAccelerator {
		t.Fatalf("row = %+v", row)
	}
	// HailoRT has no busy counter on Windows: the row must say so, or its
	// absent utilization renders as an idle "0 %". And no engine runs on it.
	if !row.UtilizationUnavailable {
		t.Error("UtilizationUnavailable unset on a Hailo row")
	}
	if row.InferenceReady == nil || *row.InferenceReady {
		t.Errorf("InferenceReady = %v, want an explicit false", row.InferenceReady)
	}
	if noderec.MaxGPUUtilization([]noderec.GPUInfo{{Kind: row.Kind, UtilizationPercent: 100}}) != 0 {
		t.Fatal("an accelerator row must not contribute to GPU pressure")
	}
}

// TestMergeAccelStatsDoesNotValidateTelemetry pins the collector-side fold:
// the sampler's temperature lands under its statsKey, the previously
// published GPU map is cloned rather than mutated, and GPUSampledAt is left
// exactly as the GPU path set it.
func TestMergeAccelStatsDoesNotValidateTelemetry(t *testing.T) {
	sampler := newHailoSampler("hailo:0000:03:00.0", "0000:03:00.0", nil)
	published := gpuStat{TemperatureC: 52}
	sampler.latest.Store(&published)

	c := &statsCollector{accels: []*hailoSampler{sampler}}
	previous := map[string]gpuStat{"luid_0x00000000_0x000054f0_phys_0": {UtilizationPct: 12}}
	snap := &statsSnapshot{GPU: previous}
	c.mergeAccelStats(snap)

	if len(previous) != 1 {
		t.Fatalf("the previously published map was mutated: %v", previous)
	}
	got, ok := snap.GPU["hailo:0000:03:00.0"]
	if !ok || got.TemperatureC != 52 {
		t.Fatalf("accelerator sample = (%+v, %v), want 52 C", got, ok)
	}
	if snap.GPU["luid_0x00000000_0x000054f0_phys_0"].UtilizationPct != 12 {
		t.Fatal("the GPU rows were lost in the merge")
	}
	if !snap.GPUSampledAt.IsZero() {
		t.Fatal("an accelerator sample validated GPU telemetry")
	}

	// A sampler that has published nothing yet adds no entry at all.
	silent := newHailoSampler("hailo:0000:04:00.0", "0000:04:00.0", nil)
	c.accels = append(c.accels, silent)
	snap = &statsSnapshot{}
	c.mergeAccelStats(snap)
	if _, ok := snap.GPU["hailo:0000:04:00.0"]; ok {
		t.Fatal("a sampler with no reading published an entry")
	}
}

// TestMergeAccelStatsNoDevices pins that a host with no accelerator leaves the
// snapshot untouched, including a nil GPU map.
func TestMergeAccelStatsNoDevices(t *testing.T) {
	c := &statsCollector{}
	snap := &statsSnapshot{}
	c.mergeAccelStats(snap)
	if snap.GPU != nil {
		t.Fatalf("GPU map = %v, want nil", snap.GPU)
	}
}

// TestHailoErrorMessage pins the diagnostic string a failed call produces:
// the entry point, the numeric hailo_status, and the library's own text when
// it is available. This is what a live failure on an unfamiliar host is read
// from, so it must never degrade to "an error occurred".
func TestHailoErrorMessage(t *testing.T) {
	err := &hailoError{call: "hailo_create_device_by_id", status: 74, message: "HAILO_DEVICE_IN_USE"}
	if got, want := err.Error(), "hailo_create_device_by_id: hailo_status 74 (HAILO_DEVICE_IN_USE)"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	err = &hailoError{call: "hailo_scan_devices", status: 8}
	if got, want := err.Error(), "hailo_scan_devices: hailo_status 8"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestNewHailoDeviceID(t *testing.T) {
	id, err := newHailoDeviceID("0000:03:00.0")
	if err != nil {
		t.Fatalf("newHailoDeviceID: %v", err)
	}
	if got := id.String(); got != "0000:03:00.0" {
		t.Errorf("String() = %q", got)
	}
	if id.ID[len("0000:03:00.0")] != 0 {
		t.Error("device id is not NUL-terminated")
	}
	// An id that would not fit is rejected rather than truncated into some
	// other device's id.
	if _, err := newHailoDeviceID(string(make([]byte, 32))); err == nil {
		t.Error("an over-long device id was accepted")
	}
	// A full 32-byte id with no room for a terminator is the same case.
	full := hailoDeviceID{}
	for i := range full.ID {
		full.ID[i] = 'a'
	}
	if got := len(full.String()); got != 32 {
		t.Errorf("unterminated id length = %d, want 32", got)
	}
}
