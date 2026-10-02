// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows side of the Hailo activity estimate (hailo_activity.go): find the
// processes that have libhailort.dll loaded and read their I/O counters.

const hailoLibName = "libhailort.dll"

// hailoNegativeTTL is how long a process found without libhailort.dll is not
// looked at again. It keeps the scan from re-opening every process on the box
// with PROCESS_VM_READ every 5 s; the cost is that a process which loads the
// library late is picked up up to this long after it does.
const hailoNegativeTTL = 60 * time.Second

// hailoSkipImages are never opened for their module list: system processes
// that cannot load HailoRT and whose memory a SYSTEM service has no business
// reading (lsass above all; security tools flag that pattern).
var hailoSkipImages = map[string]bool{
	"system": true, "registry": true, "smss.exe": true, "csrss.exe": true, "wininit.exe": true,
	"winlogon.exe": true, "services.exe": true, "lsass.exe": true, "lsaiso.exe": true,
	"svchost.exe": true, "fontdrvhost.exe": true, "dwm.exe": true, "msmpeng.exe": true,
	"nissrv.exe": true, "mssense.exe": true, "securityhealthservice.exe": true, "memory compression": true,
}

var procGetProcessIoCounters = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetProcessIoCounters")

// ioCounters is the Win32 IO_COUNTERS structure.
type ioCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

// hailoRuntimeInstalled gates the whole monitor: a host without HailoRT never
// scans its process list. Checked once at service start: a HailoRT installed
// later is monitored after the next service restart (its installer reboots).
// The installer's default location is checked, plus %HAILORT_DIR%.
func hailoRuntimeInstalled() bool {
	var candidates []string
	if dir := strings.TrimSpace(os.Getenv("HAILORT_DIR")); dir != "" {
		candidates = append(candidates, filepath.Join(dir, "bin", hailoLibName), filepath.Join(dir, hailoLibName))
	}
	pf := strings.TrimSpace(os.Getenv("ProgramFiles"))
	if pf == "" {
		pf = `C:\Program Files`
	}
	candidates = append(candidates, filepath.Join(pf, "HailoRT", "bin", hailoLibName))
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return true
		}
	}
	return false
}

// startHailoMonitor starts sampling when HailoRT is installed and returns nil
// otherwise. It stops when ctx ends. A panic inside it ends the estimate only:
// it is recovered and logged, so the CPU readings this service exists for keep
// flowing.
func startHailoMonitor(ctx context.Context, log *slog.Logger) *hailoMonitor {
	if !hailoRuntimeInstalled() {
		log.Debug("HailoRT not installed; no Hailo activity estimate")
		return nil
	}
	sc := newHailoScanner()
	m := newHailoMonitor(sc.scan, time.Now, log)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error("Hailo activity estimate stopped after a panic; CPU readings unaffected",
					"panic", r, "stack", string(debug.Stack()))
				m.latest.Store(nil)
			}
		}()
		defer m.closeAll()
		t := time.NewTicker(hailoSlot)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.slot()
			}
		}
	}()
	log.Info("Hailo activity estimate running", "slot_ms", hailoSlot.Milliseconds(), "window_slots", hailoWindowSlots)
	return m
}

type procKey struct {
	pid     uint32
	created int64
}

// hailoScanner remembers which process instances were already found not to
// load HailoRT, so they are not re-opened with PROCESS_VM_READ every scan.
type hailoScanner struct {
	negative map[procKey]time.Time
}

func newHailoScanner() *hailoScanner { return &hailoScanner{negative: map[procKey]time.Time{}} }

// scan lists the processes that have libhailort.dll loaded, except PAIR's own,
// this one, and the system processes above. Each comes with an open handle
// that can read its I/O counters; a process that cannot be inspected is
// skipped for this scan only.
func (s *hailoScanner) scan() hailoScan {
	pids, ok := allPIDs()
	if !ok {
		return hailoScan{}
	}
	now := time.Now()
	self := uint32(os.Getpid())
	alive := make(map[uint32]bool, len(pids))
	seen := make(map[procKey]bool, len(pids))
	var found []hailoProc
	for _, pid := range pids {
		alive[pid] = true
		if pid == 0 || pid == 4 || pid == self {
			continue
		}
		key, image, ok := identify(pid)
		if !ok || image == "" || hailoSkipImages[strings.ToLower(image)] || isPAIRProcess(image) {
			continue
		}
		seen[key] = true
		if until, neg := s.negative[key]; neg && now.Before(until) {
			continue
		}
		p, loaded, ok := openHailoProcess(pid, image)
		if !ok {
			continue // transient (access, partial copy): look again next scan
		}
		if !loaded {
			s.negative[key] = now.Add(hailoNegativeTTL)
			continue
		}
		found = append(found, p)
	}
	for k := range s.negative {
		if !seen[k] {
			delete(s.negative, k)
		}
	}
	return hailoScan{found: found, alive: alive, ok: true}
}

// allPIDs returns every process id, growing the buffer until it fits.
func allPIDs() ([]uint32, bool) {
	size := 1024
	for size <= 1<<20 {
		pids := make([]uint32, size)
		var got uint32
		if err := windows.EnumProcesses(pids, &got); err != nil {
			return nil, false
		}
		n := int(got) / int(unsafe.Sizeof(pids[0]))
		if n < size {
			return pids[:n], true
		}
		size *= 2
	}
	return nil, false
}

// identify reads a process's creation time and image name with the least
// access that allows it (no memory read).
func identify(pid uint32) (procKey, string, bool) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return procKey{}, "", false
	}
	defer windows.CloseHandle(h)
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return procKey{}, "", false
	}
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		return procKey{}, "", false
	}
	return procKey{pid: pid, created: created.Nanoseconds()}, filepath.Base(windows.UTF16ToString(buf[:n])), true
}

// openHailoProcess opens the process for its module list. loaded reports
// whether libhailort.dll is among the modules; ok is false when the process
// could not be inspected at all. A process that loads it is returned with the
// handle kept open for the counter reads.
func openHailoProcess(pid uint32, image string) (p hailoProc, loaded, ok bool) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ, false, pid)
	if err != nil {
		return hailoProc{}, false, false
	}
	loaded, ok = processLoadsHailo(h)
	if !ok || !loaded {
		windows.CloseHandle(h)
		return hailoProc{}, loaded, ok
	}
	return hailoProc{
		pid:   pid,
		name:  image,
		ops:   func() (uint64, error) { return processOtherOps(h) },
		close: func() { windows.CloseHandle(h) },
	}, true, true
}

// processLoadsHailo reports whether libhailort.dll is among the process's
// modules, growing the module buffer until the whole list fits.
func processLoadsHailo(h windows.Handle) (loaded, ok bool) {
	size := 512
	var mods []windows.Handle
	count := 0
	for {
		mods = make([]windows.Handle, size)
		var need uint32
		if err := windows.EnumProcessModulesEx(h, &mods[0], uint32(len(mods))*uint32(unsafe.Sizeof(mods[0])), &need, windows.LIST_MODULES_ALL); err != nil {
			return false, false
		}
		count = int(need / uint32(unsafe.Sizeof(mods[0])))
		if count <= len(mods) || size >= 1<<16 {
			break
		}
		size = count + 64
	}
	if count > len(mods) {
		count = len(mods)
	}
	buf := make([]uint16, windows.MAX_PATH)
	for _, m := range mods[:count] {
		if err := windows.GetModuleBaseName(h, m, &buf[0], uint32(len(buf))); err != nil {
			continue
		}
		if strings.EqualFold(windows.UTF16ToString(buf), hailoLibName) {
			return true, true
		}
	}
	return false, true
}

// processOtherOps reads the cumulative non-read/non-write I/O operation count
// (DeviceIoControl calls among them). A process that has exited is an error,
// so the monitor stops watching it.
func processOtherOps(h windows.Handle) (uint64, error) {
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return 0, err
	}
	if code != 259 { // STILL_ACTIVE
		return 0, windows.ERROR_PROCESS_ABORTED
	}
	var c ioCounters
	r, _, err := procGetProcessIoCounters.Call(uintptr(h), uintptr(unsafe.Pointer(&c)))
	if r == 0 {
		return 0, err
	}
	return c.OtherOperationCount, nil
}
