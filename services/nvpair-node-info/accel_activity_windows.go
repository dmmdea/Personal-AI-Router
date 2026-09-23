// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"
)

// Windows side of the accelerator activity file (accel_activity.go): where it
// lives, how it is read without getting in the writer's way, and how a stale
// file's writer is checked for liveness.

const (
	// accelActivityDirEnv overrides the directory the activity file is read
	// from. The writer honours the same variable.
	accelActivityDirEnv = "NVPAIR_ACCEL_ACTIVITY_DIR"

	// hailoActivityFile is the Hailo writer's file name inside that directory.
	hailoActivityFile = "hailo.json"

	// processStillActive is GetExitCodeProcess's STILL_ACTIVE: the process
	// has not exited.
	processStillActive = 259

	// writerStartSlack absorbs clock granularity when a live pid's creation
	// time is compared with the writer's started_ms: the writer's process
	// always exists before its tracker starts, so only a process created
	// clearly after started_ms is a different one that reused the pid.
	writerStartSlack = time.Second
)

// accelActivityDir is the directory the activity file lives in:
// %NVPAIR_ACCEL_ACTIVITY_DIR% when set, else %ProgramData%\nvpair\accel-activity.
func accelActivityDir(env func(string) string) string {
	if dir := strings.TrimSpace(env(accelActivityDirEnv)); dir != "" {
		return dir
	}
	programData := strings.TrimSpace(env("ProgramData"))
	if programData == "" {
		programData = `C:\ProgramData`
	}
	return filepath.Join(programData, "nvpair", "accel-activity")
}

// openSharedRead opens path for reading with every sharing flag set.
//
// The writer replaces the file by temp-and-rename while this service may be
// reading it, and what an open reader handle does to that rename depends on
// how the writer renames (measured on Windows 11 / NTFS, see
// TestOpenSharedReadAllowsReplace):
//
//   - A POSIX-semantics rename (SetFileInformationByHandle with
//     FILE_RENAME_FLAG_POSIX_SEMANTICS) succeeds while a reader holds the file
//     only if that reader opened it with FILE_SHARE_DELETE. os.Open does not
//     pass that flag, which is why this function exists: with it, the rename
//     goes through and the open handle keeps reading the copy it opened.
//   - A classic replace (MoveFileEx with MOVEFILE_REPLACE_EXISTING — Go's
//     os.Rename, Python's os.replace) fails with ERROR_ACCESS_DENIED while any
//     handle to the target is open, whatever its sharing flags. No reader-side
//     flag can prevent that, so readAccelActivityFile also holds the handle
//     for one small read only, and a writer that renames this way must treat
//     a failed replace as transient and write again on its next tick.
func openSharedRead(path string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := windows.CreateFile(p,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

// readAccelActivityFile reads the whole file and closes it at once, so the
// handle exists for one small read every sampler tick (see openSharedRead for
// what that window means to the writer's rename).
func readAccelActivityFile(path string) ([]byte, error) {
	f, err := openSharedRead(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, accelActivityMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > accelActivityMaxBytes {
		return nil, fmt.Errorf("%w: larger than %d bytes", errAccelActivityInvalid, accelActivityMaxBytes)
	}
	return data, nil
}

// accelActivityWriterAlive reports whether the process that wrote sample is
// still running. A pid that cannot be opened because it does not exist is
// gone; one that exists but has exited, or that was created after the
// writer's tracker started (the pid was reused by some later process), is not
// the writer either. A process this service may not inspect at all (access
// denied) exists, so it is treated as alive: "not reporting" is the safe
// answer when liveness cannot be settled, since it publishes no figure at all.
func accelActivityWriterAlive(sample accelActivity) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, sample.PID)
	if err != nil {
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err == nil && code != processStillActive {
		return false
	}
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err == nil {
		createdMS := float64(created.Nanoseconds() / int64(time.Millisecond))
		if createdMS > sample.StartedMS+float64(writerStartSlack.Milliseconds()) {
			return false
		}
	}
	return true
}

// hailoActivity is a Hailo sampler's source of utilization: the activity
// file, read on the sampler's own tick. Every dependency is injected so the
// sampler's use of it is testable without a writer process.
type hailoActivity struct {
	read  func() ([]byte, error)
	alive func(accelActivity) bool
	now   func() time.Time

	tracker   accelActivityTracker
	lastState accelActivityState
	logged    bool
}

// newHailoActivity is the live source: the file under accelActivityDir, the
// real process table and the wall clock.
func newHailoActivity(env func(string) string) *hailoActivity {
	path := filepath.Join(accelActivityDir(env), hailoActivityFile)
	return &hailoActivity{
		read:  func() ([]byte, error) { return readAccelActivityFile(path) },
		alive: accelActivityWriterAlive,
		now:   time.Now,
	}
}

// sample reads the file once and returns the utilization to publish; known
// is false when there is nothing to publish. It logs one line per state
// change and none per tick.
func (a *hailoActivity) sample(device string) (pct uint32, known bool) {
	var parsed accelActivity
	data, err := a.read()
	if err == nil {
		parsed, err = parseAccelActivity(data, accelActivityDeviceHailo8L)
	}
	pct, known, state := a.tracker.observe(parsed, err, a.now(), a.alive)
	if !a.logged || state != a.lastState {
		a.logState(device, state, err)
		a.lastState, a.logged = state, true
	}
	return pct, known
}

func (a *hailoActivity) logState(device string, state accelActivityState, err error) {
	switch state {
	case activityAbsent:
		slog.Debug("Hailo activity file absent; utilization unavailable", "device", device)
	case activityInvalid:
		slog.Warn("Hailo activity file unusable; utilization unavailable", "device", device, "err", err)
	case activityPending:
		slog.Info("Hailo activity writer found; utilization from its next sample", "device", device)
	case activityFresh:
		slog.Info("Hailo utilization is being reported by the activity writer", "device", device)
	case activityWriterGone:
		slog.Info("Hailo activity writer has exited; reporting the device idle", "device", device)
	case activityWriterSilent:
		slog.Warn("Hailo activity writer is running but has stopped reporting; utilization unavailable",
			"device", device)
	}
}
