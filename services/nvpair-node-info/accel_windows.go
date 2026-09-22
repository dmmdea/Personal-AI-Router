// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"nvpair-shared/noderec"
)

// Windows inference-accelerator detection and sampling (Hailo).
//
// The GPU inventory comes from DXGI, which only ever sees display adapters: a
// Hailo M.2 module sits on PCIe as its own accelerator function behind the
// vendor PCIe driver and never appears there. This file lists those devices in
// the same inventory (Kind = noderec.GPUKindAccelerator), exactly as
// accel_linux.go does for the gasket/apex Edge TPU.
//
// The device is reached through HailoRT's C API (libhailort.dll) with
// LazyDLL, no cgo:
//
//   - inventory   : hailo_scan_devices gives one id (PCIe BDF) per module;
//     hailo_identify on a briefly opened handle gives the device
//     architecture, which names the row.
//   - temperature : hailo_get_chip_temperature returns the two on-die sensors
//     (TS0/TS1) and the number of samples behind them. The reported figure is
//     the hotter of the two, rounded.
//
// What this deliberately does NOT report, because HailoRT 4.24 on Windows
// cannot supply it:
//
//   - utilization: there is no busy counter in the API and the runtime's
//     monitor mode is unsupported on this platform, so no utilization_percent
//     is published. A literal 0 would read as "idle" and would be a lie.
//   - power: power measurement is unsupported on the M.2 Hailo-8L module, so
//     hailo_power_measurement is not called at all.
//
// Nothing here counts toward GPUSampledAt / TelemetryValid: an accelerator
// cannot run PAIR's engines, so it must not make a GPU-less host look like it
// has fresh GPU telemetry, and noderec.MaxGPUUtilization skips these rows.

const (
	// hailoSampleInterval matches the other Windows out-of-band pollers
	// (nvidia-smi GPU temperature, the host-sensor CPU package reading): the
	// reading is a slow-moving thermal value and each poll is a firmware
	// control transaction, so there is nothing to gain from the 1 s tick.
	hailoSampleInterval = 5 * time.Second

	// hailoReopenAfterFailures is how many consecutive failed reads drop the
	// device handle so the next sample opens a fresh one. A driver restart or
	// a surprise removal-and-return invalidates the handle permanently and
	// every subsequent read on it fails; reopening is the only recovery. Same
	// rule the CPU package poller applies to its helper connection.
	hailoReopenAfterFailures = 3

	// hailoDLLName is the HailoRT runtime library.
	hailoDLLName = "libhailort.dll"

	// hailoGenericName labels a Hailo device whose architecture we could not
	// establish. Listing it unnamed-but-present beats dropping it.
	hailoGenericName = "Hailo AI Accelerator"

	// hailoPCIVendor is Hailo's PCI vendor id as Windows spells it in the PnP
	// enumerator key.
	hailoPCIVendor = "VEN_1E60"

	// pciEnumKey is the PnP enumerator branch every PCI function is listed
	// under: <pciEnumKey>\<VEN_xxxx&DEV_xxxx&...>\<instance>.
	pciEnumKey = `SYSTEM\CurrentControlSet\Enum\PCI`
)

// hailo_status values this file reacts to by name (hailort.h). The status is a
// C enum, i.e. a 32-bit int, with 0 for success.
const (
	hailoSuccess            int32 = 0
	hailoInsufficientBuffer int32 = 5
)

// hailo_device_architecture_t values (hailort.h).
const (
	hailoArchHailo8A0 uint32 = iota
	hailoArchHailo8
	hailoArchHailo8L
	hailoArchHailo15H
	hailoArchHailo15L
	hailoArchHailo15M
	hailoArchHailo10H
	hailoArchMars
	hailoArchCount
)

// hailoArchNames maps a device architecture to the inventory row's name.
var hailoArchNames = map[uint32]string{
	hailoArchHailo8A0: "Hailo-8 AI Accelerator",
	hailoArchHailo8:   "Hailo-8 AI Accelerator",
	hailoArchHailo8L:  "Hailo-8L AI Accelerator",
	hailoArchHailo15H: "Hailo-15H AI Accelerator",
	hailoArchHailo15L: "Hailo-15L AI Accelerator",
	hailoArchHailo15M: "Hailo-15M AI Accelerator",
	hailoArchHailo10H: "Hailo-10H AI Accelerator",
}

// hailoPCIDeviceNames names a module by its PCI device id, for the presence
// fallback that runs without the runtime library. The M.2 A+E module this was
// measured against enumerates as VEN_1E60&DEV_2864.
var hailoPCIDeviceNames = map[uint16]string{
	0x2864: "Hailo-8L AI Accelerator",
}

// hailoArchName names a row from hailo_identify's device_architecture. An
// architecture this build does not know still yields a usable generic label.
func hailoArchName(arch uint32) string {
	if name, ok := hailoArchNames[arch]; ok {
		return name
	}
	return hailoGenericName
}

// hailoPCIDeviceName names a row from the PnP device id.
func hailoPCIDeviceName(device uint16) string {
	if name, ok := hailoPCIDeviceNames[device]; ok {
		return name
	}
	return hailoGenericName
}

// hailoStatsKey is the statsKey that joins a static accelerator row to its
// sampler's snapshot entry. Namespaced so it can never collide with the PDH
// LUID keys the GPU rows use.
func hailoStatsKey(id string) string { return "hailo:" + strings.ToLower(strings.TrimSpace(id)) }

// ---------------------------------------------------------------------------
// Library location and binding
// ---------------------------------------------------------------------------

// hailoDLLCandidates lists, in order, where libhailort.dll may live. An
// explicit HAILORT_DIR / HAILORT_ROOT wins (both the directory itself and its
// bin subdirectory are tried, since installs disagree about which one the
// variable names), then the default install location, and finally the bare
// name so the loader's own search path gets a turn.
func hailoDLLCandidates(env func(string) string) []string {
	var out []string
	seen := map[string]struct{}{}
	add := func(p string) {
		if p == "" {
			return
		}
		if _, dup := seen[strings.ToLower(p)]; dup {
			return
		}
		seen[strings.ToLower(p)] = struct{}{}
		out = append(out, p)
	}
	addDir := func(dir string) {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			return
		}
		add(filepath.Join(dir, hailoDLLName))
		add(filepath.Join(dir, "bin", hailoDLLName))
	}
	addDir(env("HAILORT_DIR"))
	addDir(env("HAILORT_ROOT"))
	programFiles := strings.TrimSpace(env("ProgramFiles"))
	if programFiles == "" {
		programFiles = `C:\Program Files`
	}
	add(filepath.Join(programFiles, "HailoRT", "bin", hailoDLLName))
	add(hailoDLLName)
	return out
}

// resolveHailoDLL picks the first candidate that exists on disk. When none
// does it returns the bare library name and lets the loader decide, which is
// also the case that produces the ordinary "no HailoRT installed" error.
func resolveHailoDLL(env func(string) string, exists func(string) bool) string {
	for _, c := range hailoDLLCandidates(env) {
		if c == hailoDLLName {
			continue
		}
		if exists(c) {
			return c
		}
	}
	return hailoDLLName
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

// hailoLib is the bound subset of libhailort.dll. Only the entry points this
// service calls are resolved; a missing required one makes the whole library
// unusable rather than failing later at an arbitrary call site.
type hailoLib struct {
	path string

	scanDevicesProc     *windows.LazyProc
	createDeviceProc    *windows.LazyProc
	releaseDeviceProc   *windows.LazyProc
	chipTemperatureProc *windows.LazyProc
	identifyProc        *windows.LazyProc
	statusMessageProc   *windows.LazyProc
}

var (
	hailoLibOnce sync.Once
	hailoLibInst *hailoLib
	hailoLibErr  error
)

// hailoRuntime loads libhailort.dll once per process. A missing library is the
// normal "this host has no Hailo module" case, so it is logged at Debug and
// the error is cached: every later caller gets the same answer without
// re-attempting the load.
func hailoRuntime() (*hailoLib, error) {
	hailoLibOnce.Do(func() {
		hailoLibInst, hailoLibErr = loadHailoRT(os.Getenv, fileExists)
		if hailoLibErr != nil {
			slog.Debug("HailoRT runtime not available; no Hailo accelerator telemetry",
				"library", hailoDLLName, "err", hailoLibErr)
		} else {
			slog.Info("HailoRT runtime loaded", "path", hailoLibInst.path)
		}
	})
	return hailoLibInst, hailoLibErr
}

func loadHailoRT(env func(string) string, exists func(string) bool) (*hailoLib, error) {
	path := resolveHailoDLL(env, exists)
	dll := windows.NewLazyDLL(path)
	if err := dll.Load(); err != nil {
		return nil, fmt.Errorf("load %s: %w", path, err)
	}
	lib := &hailoLib{
		path:                path,
		scanDevicesProc:     dll.NewProc("hailo_scan_devices"),
		createDeviceProc:    dll.NewProc("hailo_create_device_by_id"),
		releaseDeviceProc:   dll.NewProc("hailo_release_device"),
		chipTemperatureProc: dll.NewProc("hailo_get_chip_temperature"),
		identifyProc:        dll.NewProc("hailo_identify"),
		statusMessageProc:   dll.NewProc("hailo_get_status_message"),
	}
	for _, p := range []*windows.LazyProc{
		lib.scanDevicesProc, lib.createDeviceProc, lib.releaseDeviceProc, lib.chipTemperatureProc,
	} {
		if err := p.Find(); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	// hailo_identify and hailo_get_status_message are optional: without them
	// rows are still listed and sampled, just with a generic name or a
	// numeric-only status in the log.
	if err := lib.identifyProc.Find(); err != nil {
		slog.Debug("hailo_identify unavailable; accelerator rows will use a generic name", "err", err)
		lib.identifyProc = nil
	}
	if err := lib.statusMessageProc.Find(); err != nil {
		lib.statusMessageProc = nil
	}
	return lib, nil
}

// hailoError carries a failed call's hailo_status so a live diagnosis can
// quote the exact code, with the library's own message text when it is
// reachable.
type hailoError struct {
	call    string
	status  int32
	message string
}

func (e *hailoError) Error() string {
	if e.message != "" {
		return fmt.Sprintf("%s: hailo_status %d (%s)", e.call, e.status, e.message)
	}
	return fmt.Sprintf("%s: hailo_status %d", e.call, e.status)
}

// statusError wraps a non-success status from one call into an error.
func (l *hailoLib) statusError(call string, status int32) error {
	return &hailoError{call: call, status: status, message: l.statusMessage(status)}
}

// statusMessage renders hailo_get_status_message's C string, or "" when the
// export is absent or returned NULL.
func (l *hailoLib) statusMessage(status int32) string {
	if l == nil || l.statusMessageProc == nil {
		return ""
	}
	r, _, _ := l.statusMessageProc.Call(uintptr(uint32(status)))
	if r == 0 {
		return ""
	}
	// The call returns a const char* in a uintptr. Reinterpreting the
	// uintptr's bits through its own address (rather than converting the
	// value directly) keeps the unsafe.Pointer rules — and go vet's
	// unsafeptr check — satisfied; the string is static storage inside the
	// library, so there is no Go object for the collector to move or free.
	return windows.BytePtrToString(*(**byte)(unsafe.Pointer(&r)))
}

// callStatus invokes a HailoRT entry point and returns its hailo_status. The
// C enum is a 32-bit int, so only the low half of the register is meaningful.
func callStatus(proc *windows.LazyProc, args ...uintptr) int32 {
	r, _, _ := proc.Call(args...)
	return int32(uint32(r))
}

// ---------------------------------------------------------------------------
// C structures (HailoRT v4.24.0 hailort.h)
// ---------------------------------------------------------------------------

// hailoDeviceID mirrors hailo_device_id_t: a fixed 32-byte NUL-terminated id
// (the PCIe BDF for a PCIe module).
type hailoDeviceID struct {
	ID [32]byte
}

// String is the id up to its NUL terminator.
func (d *hailoDeviceID) String() string {
	for i, c := range d.ID {
		if c == 0 {
			return string(d.ID[:i])
		}
	}
	return string(d.ID[:])
}

// newHailoDeviceID packs a device id string into the fixed-size C struct. An
// id that does not fit (never the case for a BDF) is rejected rather than
// silently truncated into some other device's id.
func newHailoDeviceID(id string) (hailoDeviceID, error) {
	var out hailoDeviceID
	if len(id) >= len(out.ID) {
		return out, fmt.Errorf("device id %q exceeds %d bytes", id, len(out.ID)-1)
	}
	copy(out.ID[:], id)
	return out, nil
}

// hailoFirmwareVersion mirrors hailo_firmware_version_t.
type hailoFirmwareVersion struct {
	Major    uint32
	Minor    uint32
	Revision uint32
}

// hailoDeviceIdentity mirrors hailo_device_identity_t. The explicit padding
// byte reproduces the alignment the C compiler inserts before the 4-byte
// device_architecture enum; accel_windows_test.go pins the size and the
// offsets this file reads, so a header change cannot silently shift the field.
type hailoDeviceIdentity struct {
	ProtocolVersion             uint32
	FWVersion                   hailoFirmwareVersion
	LoggerVersion               uint32
	BoardNameLength             uint8
	BoardName                   [32]byte
	IsRelease                   bool
	ExtendedContextSwitchBuffer bool
	_                           byte
	DeviceArchitecture          uint32
	SerialNumberLength          uint8
	SerialNumber                [16]byte
	PartNumberLength            uint8
	PartNumber                  [16]byte
	ProductNameLength           uint8
	ProductName                 [42]byte
}

// plausible reports whether the struct the library filled in matches the
// layout this build expects. Every length field is bounded by its own array,
// so a header that reordered or resized a field almost certainly trips one of
// them — and a nonsense architecture would otherwise be read as a real one.
func (i *hailoDeviceIdentity) plausible() bool {
	return int(i.BoardNameLength) <= len(i.BoardName) &&
		int(i.SerialNumberLength) <= len(i.SerialNumber) &&
		int(i.PartNumberLength) <= len(i.PartNumber) &&
		int(i.ProductNameLength) <= len(i.ProductName) &&
		i.DeviceArchitecture < hailoArchCount
}

// hailoChipTemperature mirrors hailo_chip_temperature_info_t: the two on-die
// sensors in Celsius plus the number of samples behind them. The trailing pad
// reproduces the struct's 4-byte alignment (12 bytes total).
type hailoChipTemperature struct {
	TS0         float32
	TS1         float32
	SampleCount uint16
	_           uint16
}

// ---------------------------------------------------------------------------
// Library calls
// ---------------------------------------------------------------------------

// scanDevices returns the id of every Hailo device in the system. An empty
// result with no error is the "runtime installed, no module fitted" case.
func (l *hailoLib) scanDevices() ([]string, error) {
	capacity := 16
	for attempt := 0; attempt < 2; attempt++ {
		ids := make([]hailoDeviceID, capacity)
		count := uintptr(capacity)
		status := callStatus(l.scanDevicesProc,
			0, // params: only NULL is allowed
			uintptr(unsafe.Pointer(&ids[0])),
			uintptr(unsafe.Pointer(&count)),
		)
		if status == hailoInsufficientBuffer && int(count) > capacity {
			capacity = int(count)
			continue
		}
		if status != hailoSuccess {
			return nil, l.statusError("hailo_scan_devices", status)
		}
		if int(count) > capacity {
			count = uintptr(capacity)
		}
		out := make([]string, 0, count)
		for i := 0; i < int(count); i++ {
			if id := ids[i].String(); id != "" {
				out = append(out, id)
			}
		}
		return out, nil
	}
	return nil, errors.New("hailo_scan_devices: reported device count kept growing")
}

// hailoDevice is one opened device handle.
type hailoDevice struct {
	lib    *hailoLib
	id     string
	handle uintptr
}

// open creates a device handle for one scanned id. The sampler holds its
// handle for its lifetime: opening per reading would pay the driver's attach
// cost every five seconds.
func (l *hailoLib) open(id string) (*hailoDevice, error) {
	cid, err := newHailoDeviceID(id)
	if err != nil {
		return nil, err
	}
	var handle uintptr
	status := callStatus(l.createDeviceProc,
		uintptr(unsafe.Pointer(&cid)),
		uintptr(unsafe.Pointer(&handle)),
	)
	if status != hailoSuccess {
		return nil, l.statusError("hailo_create_device_by_id", status)
	}
	if handle == 0 {
		return nil, errors.New("hailo_create_device_by_id: null device")
	}
	return &hailoDevice{lib: l, id: id, handle: handle}, nil
}

func (d *hailoDevice) close() {
	if d == nil || d.handle == 0 {
		return
	}
	if status := callStatus(d.lib.releaseDeviceProc, d.handle); status != hailoSuccess {
		slog.Debug("hailo_release_device failed", "device", d.id, "status", status)
	}
	d.handle = 0
}

// temperature reads both on-die sensors. samples is the firmware's sample
// count behind the reading; zero means the sensors have not been read yet and
// the values carry nothing.
func (d *hailoDevice) temperature() (ts0, ts1 float32, samples uint16, err error) {
	var info hailoChipTemperature
	status := callStatus(d.lib.chipTemperatureProc, d.handle, uintptr(unsafe.Pointer(&info)))
	if status != hailoSuccess {
		return 0, 0, 0, d.lib.statusError("hailo_get_chip_temperature", status)
	}
	return info.TS0, info.TS1, info.SampleCount, nil
}

// architecture runs the identify control and returns device_architecture. ok
// is false when the export is absent, the control failed, or the filled struct
// does not match the layout this build compiled against.
func (d *hailoDevice) architecture() (uint32, bool) {
	if d.lib.identifyProc == nil {
		return 0, false
	}
	var identity hailoDeviceIdentity
	status := callStatus(d.lib.identifyProc, d.handle, uintptr(unsafe.Pointer(&identity)))
	if status != hailoSuccess {
		slog.Debug("hailo_identify failed", "device", d.id,
			"err", d.lib.statusError("hailo_identify", status))
		return 0, false
	}
	if !identity.plausible() {
		slog.Warn("hailo_identify returned an implausible identity; naming the accelerator generically",
			"device", d.id, "architecture", identity.DeviceArchitecture)
		return 0, false
	}
	return identity.DeviceArchitecture, true
}

// ---------------------------------------------------------------------------
// PnP presence fallback
// ---------------------------------------------------------------------------

// hailoPnPDeviceID extracts the PCI device id from a PnP enumerator key name
// such as "VEN_1E60&DEV_2864&SUBSYS_28641E60&REV_01". ok is false for any key
// that is not a Hailo function.
func hailoPnPDeviceID(key string) (uint16, bool) {
	var vendorOK, deviceOK bool
	var device uint16
	for _, f := range strings.Split(strings.ToUpper(strings.TrimSpace(key)), "&") {
		switch {
		case f == hailoPCIVendor:
			vendorOK = true
		case strings.HasPrefix(f, "DEV_"):
			v, err := strconv.ParseUint(strings.TrimPrefix(f, "DEV_"), 16, 16)
			if err != nil {
				continue
			}
			device, deviceOK = uint16(v), true
		}
	}
	if !vendorOK || !deviceOK {
		return 0, false
	}
	return device, true
}

// subKeyLister enumerates a registry key's immediate children. Injected so the
// presence scan is unit-testable without a Hailo module or a live registry.
type subKeyLister func(path string) ([]string, error)

// listRegistrySubKeys is the live subKeyLister over HKLM.
func listRegistrySubKeys(path string) ([]string, error) {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, path, registry.READ)
	if err != nil {
		return nil, err
	}
	defer key.Close()
	return key.ReadSubKeyNames(-1)
}

// hailoPresenceAccelerators lists one row per Hailo PCI function the PnP
// enumerator knows about. It is the fallback for a host where the module is
// fitted and its driver is bound but HailoRT is not installed, so no
// temperature can be read — presence and a name are still worth reporting.
//
// Caveat by construction: the enumerator branch also retains an entry for a
// module that has since been removed, so this path can list a device that is
// no longer fitted. The library scan, which talks to the hardware, is always
// preferred and this runs only when that is impossible.
func hailoPresenceAccelerators(list subKeyLister) []GPUInfo {
	deviceKeys, err := list(pciEnumKey)
	if err != nil {
		slog.Debug("PCI enumerator unreadable; no accelerator presence fallback",
			"key", pciEnumKey, "err", err)
		return nil
	}
	var out []GPUInfo
	for _, deviceKey := range deviceKeys {
		device, ok := hailoPnPDeviceID(deviceKey)
		if !ok {
			continue
		}
		name := hailoPCIDeviceName(device)
		instances, err := list(pciEnumKey + `\` + deviceKey)
		if err != nil || len(instances) == 0 {
			// The device key alone is evidence of one function.
			out = append(out, hailoRow(name, hailoStatsKey("pnp:"+deviceKey)))
			continue
		}
		for _, instance := range instances {
			out = append(out, hailoRow(name, hailoStatsKey("pnp:"+deviceKey+`\`+instance)))
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Inventory
// ---------------------------------------------------------------------------

// detectAccelerators returns one inventory row per Hailo device. The dynamic
// temperature field is filled by buildResponse from the samplers, joined by
// statsKey.
func detectAccelerators() []GPUInfo {
	lib, err := hailoRuntime()
	if err != nil {
		return hailoPresenceAccelerators(listRegistrySubKeys)
	}
	ids, err := lib.scanDevices()
	if err != nil {
		slog.Warn("hailo_scan_devices failed; falling back to the PnP enumerator", "err", err)
		return hailoPresenceAccelerators(listRegistrySubKeys)
	}
	if len(ids) == 0 {
		return nil
	}
	return lib.acceleratorRows(ids, listRegistrySubKeys)
}

// acceleratorRows names each scanned device. The architecture reported by the
// device itself is authoritative; when identify is unavailable the PnP
// enumerator can still name an unambiguous single module, and only then is the
// generic label used.
func (l *hailoLib) acceleratorRows(ids []string, list subKeyLister) []GPUInfo {
	out := make([]GPUInfo, 0, len(ids))
	var pnp []GPUInfo
	pnpLoaded := false
	for _, id := range ids {
		name := hailoGenericName
		if arch, ok := l.deviceArchitecture(id); ok {
			name = hailoArchName(arch)
		} else {
			if !pnpLoaded {
				pnp, pnpLoaded = hailoPresenceAccelerators(list), true
			}
			// Only an unambiguous pairing can be trusted: the scan order
			// (BDF) and the enumerator's key order are unrelated.
			if len(ids) == 1 && len(pnp) == 1 {
				name = pnp[0].Name
			}
		}
		out = append(out, hailoRow(name, hailoStatsKey(id)))
	}
	return out
}

// hailoRow is one Hailo module's inventory row, whichever path found it. The
// row says outright that it has no utilization source (HailoRT exposes no busy
// counter on Windows, see the top of this file) and that no engine can use it,
// so a client renders neither an invented "0 %" nor an inference device.
func hailoRow(name, statsKey string) GPUInfo {
	return GPUInfo{
		Name:                   name,
		Kind:                   noderec.GPUKindAccelerator,
		UtilizationUnavailable: true,
		InferenceReady:         notInferenceReady(),
		statsKey:               statsKey,
	}
}

// deviceArchitecture opens one device just long enough to identify it. The
// handle is released immediately so the sampler can take its own.
func (l *hailoLib) deviceArchitecture(id string) (uint32, bool) {
	dev, err := l.open(id)
	if err != nil {
		slog.Warn("Hailo device could not be opened to identify it", "device", id, "err", err)
		return 0, false
	}
	defer dev.close()
	return dev.architecture()
}

// ---------------------------------------------------------------------------
// Sampler
// ---------------------------------------------------------------------------

// hailoTemperature picks the figure to report from the two on-die sensors.
// Both are die temperatures, so the hotter one is the device's temperature —
// the same choice nvidia-smi makes across a GPU's sensors. ok is false when
// the firmware has taken no sample yet or the values are not a usable
// temperature, and the field is then omitted rather than reported as zero.
func hailoTemperature(ts0, ts1 float32, samples uint16) (uint32, bool) {
	if samples == 0 {
		return 0, false
	}
	hottest := float64(ts0)
	if float64(ts1) > hottest {
		hottest = float64(ts1)
	}
	if math.IsNaN(hottest) || hottest <= 0 || hottest > 200 {
		return 0, false
	}
	return uint32(math.Round(hottest)), true
}

// hailoTempReader reads the two on-die sensors of an already-open device.
type hailoTempReader func() (ts0, ts1 float32, samples uint16, err error)

// hailoOpener opens one device and returns its reader together with the
// closure that releases the handle. Injected so the sampler's reopen rule is
// testable without the library.
type hailoOpener func() (hailoTempReader, func(), error)

// hailoSampler polls one device in its own goroutine and publishes the latest
// gpuStat atomically; the collector's 1 s tick only reads it. Utilization is
// never published — HailoRT exposes no busy counter on Windows.
type hailoSampler struct {
	key string
	id  string

	// Sampler-goroutine state; never touched by other goroutines.
	open     hailoOpener
	read     hailoTempReader
	release  func()
	failures int
	lastNote string

	latest atomic.Pointer[gpuStat]
	stop   chan struct{}
	done   chan struct{}
}

func newHailoSampler(key, id string, open hailoOpener) *hailoSampler {
	return &hailoSampler{
		key:  key,
		id:   id,
		open: open,
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
}

// startHailoSamplers spins up one sampler per scanned device. Returns nil when
// the runtime or the hardware is absent, so callers can range over it
// unconditionally.
func startHailoSamplers() []*hailoSampler {
	lib, err := hailoRuntime()
	if err != nil {
		return nil
	}
	ids, err := lib.scanDevices()
	if err != nil {
		slog.Warn("hailo_scan_devices failed; no accelerator temperature will be reported", "err", err)
		return nil
	}
	var samplers []*hailoSampler
	for _, id := range ids {
		device := id
		s := newHailoSampler(hailoStatsKey(device), device, func() (hailoTempReader, func(), error) {
			dev, err := lib.open(device)
			if err != nil {
				return nil, nil, err
			}
			return dev.temperature, dev.close, nil
		})
		go s.run()
		samplers = append(samplers, s)
	}
	return samplers
}

// sample performs one poll: open the device if it is not open, read both
// sensors, publish. Split out from run so the reopen rule is unit-testable.
func (s *hailoSampler) sample() {
	if s.read == nil {
		read, release, err := s.open()
		if err != nil {
			s.note("open: "+err.Error(),
				"Hailo device cannot be opened; its temperature_celsius will be omitted",
				"device", s.id, "err", err)
			return
		}
		s.read, s.release, s.failures = read, release, 0
	}
	ts0, ts1, samples, err := s.read()
	if err != nil {
		s.failures++
		s.note("read: "+err.Error(), "Hailo chip temperature read failed",
			"device", s.id, "err", err)
		if s.failures >= hailoReopenAfterFailures {
			// The handle does not survive a driver restart or a surprise
			// removal; drop it so the next tick opens a fresh one.
			slog.Info("reopening Hailo device after consecutive read failures",
				"device", s.id, "failures", s.failures)
			s.closeDevice()
		}
		return
	}
	s.failures = 0
	c, ok := hailoTemperature(ts0, ts1, samples)
	if !ok {
		return
	}
	st := gpuStat{TemperatureC: c}
	s.latest.Store(&st)
	s.lastNote = ""
}

// closeDevice releases the handle and forces the next sample to reopen.
func (s *hailoSampler) closeDevice() {
	if s.release != nil {
		s.release()
	}
	s.read, s.release, s.failures = nil, nil, 0
}

// note logs msg once per distinct key, so a steady failure costs one line.
func (s *hailoSampler) note(key, msg string, args ...any) {
	if s.lastNote == key {
		return
	}
	s.lastNote = key
	slog.Log(context.Background(), slog.LevelWarn, msg, args...)
}

func (s *hailoSampler) run() {
	defer close(s.done)
	defer s.closeDevice()
	ticker := time.NewTicker(hailoSampleInterval)
	defer ticker.Stop()
	s.sample()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			s.sample()
		}
	}
}

// Latest returns the most recent published sample, or false before the first.
func (s *hailoSampler) Latest() (gpuStat, bool) {
	p := s.latest.Load()
	if p == nil {
		return gpuStat{}, false
	}
	return *p, true
}

func (s *hailoSampler) Stop() {
	close(s.stop)
	<-s.done
}
