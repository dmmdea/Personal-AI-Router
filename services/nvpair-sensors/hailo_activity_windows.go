// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows side of the Hailo activity estimate (hailo_activity.go): find the
// processes that have libhailort.dll loaded and read their I/O counters.

const hailoLibName = "libhailort.dll"

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
// scans its process list. The installer's default location is checked, plus
// %HAILORT_DIR% for a relocated install.
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
// otherwise. It stops when ctx ends.
func startHailoMonitor(ctx context.Context, log *slog.Logger) *hailoMonitor {
	if !hailoRuntimeInstalled() {
		log.Debug("HailoRT not installed; no Hailo activity estimate")
		return nil
	}
	m := newHailoMonitor(scanHailoProcesses, time.Now, log)
	go func() {
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

// scanHailoProcesses lists the processes that have libhailort.dll loaded,
// except PAIR's own and this one. Each comes with an open handle that can
// read its I/O counters; a process this service cannot open is skipped.
func scanHailoProcesses() []hailoProc {
	pids := make([]uint32, 2048)
	var got uint32
	if err := windows.EnumProcesses(pids, &got); err != nil {
		return nil
	}
	n := int(got) / int(unsafe.Sizeof(pids[0]))
	self := uint32(os.Getpid())
	var out []hailoProc
	for _, pid := range pids[:n] {
		if pid == 0 || pid == self {
			continue
		}
		if p, ok := openHailoProcess(pid); ok {
			out = append(out, p)
		}
	}
	return out
}

func openHailoProcess(pid uint32) (hailoProc, bool) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ, false, pid)
	if err != nil {
		return hailoProc{}, false
	}
	name, loaded := processLoadsHailo(h)
	if !loaded || isPAIRProcess(name) {
		windows.CloseHandle(h)
		return hailoProc{}, false
	}
	return hailoProc{
		pid:   pid,
		name:  name,
		ops:   func() (uint64, error) { return processOtherOps(h) },
		close: func() { windows.CloseHandle(h) },
	}, true
}

// processLoadsHailo returns the process's image name and whether
// libhailort.dll is among its modules.
func processLoadsHailo(h windows.Handle) (string, bool) {
	mods := make([]windows.Handle, 1024)
	var need uint32
	if err := windows.EnumProcessModulesEx(h, &mods[0], uint32(len(mods))*uint32(unsafe.Sizeof(mods[0])), &need, windows.LIST_MODULES_ALL); err != nil {
		return "", false
	}
	count := int(need / uint32(unsafe.Sizeof(mods[0])))
	if count > len(mods) {
		count = len(mods)
	}
	buf := make([]uint16, windows.MAX_PATH)
	name := ""
	if count > 0 {
		if err := windows.GetModuleBaseName(h, mods[0], &buf[0], uint32(len(buf))); err == nil {
			name = windows.UTF16ToString(buf)
		}
	}
	for _, m := range mods[1:count] {
		if err := windows.GetModuleBaseName(h, m, &buf[0], uint32(len(buf))); err != nil {
			continue
		}
		if strings.EqualFold(windows.UTF16ToString(buf), hailoLibName) {
			return name, true
		}
	}
	return name, false
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
