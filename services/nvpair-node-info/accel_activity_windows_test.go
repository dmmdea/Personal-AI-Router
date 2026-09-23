// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestAccelActivityDir(t *testing.T) {
	env := func(vals map[string]string) func(string) string {
		return func(k string) string { return vals[k] }
	}
	if got := accelActivityDir(env(map[string]string{
		accelActivityDirEnv: ` D:\custom\activity `, "ProgramData": `C:\ProgramData`,
	})); got != `D:\custom\activity` {
		t.Errorf("override: %q", got)
	}
	if got := accelActivityDir(env(map[string]string{"ProgramData": `E:\PD`})); got != `E:\PD\nvpair\accel-activity` {
		t.Errorf("ProgramData default: %q", got)
	}
	if got := accelActivityDir(env(nil)); got != `C:\ProgramData\nvpair\accel-activity` {
		t.Errorf("no environment: %q", got)
	}
}

// posixRenameOver replaces dst with src the way a POSIX-semantics writer
// does: SetFileInformationByHandle(FileRenameInfoEx) with
// FILE_RENAME_FLAG_REPLACE_IF_EXISTS | FILE_RENAME_FLAG_POSIX_SEMANTICS.
func posixRenameOver(src, dst string) error {
	const (
		fileRenameInfoEx        = 22
		renameReplaceIfExists   = 0x1
		renamePosixSemantics    = 0x2
		fileRenameInfoNameStart = 20 // Flags(4) + pad(4) + RootDirectory(8) + FileNameLength(4) on 64-bit
	)
	if unsafe.Sizeof(uintptr(0)) != 8 {
		return errors.New("posixRenameOver lays out FILE_RENAME_INFO for 64-bit only")
	}
	p, err := windows.UTF16PtrFromString(src)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(p, windows.DELETE|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	name, err := windows.UTF16FromString(dst)
	if err != nil {
		return err
	}
	buf := make([]byte, fileRenameInfoNameStart+len(name)*2)
	binary.LittleEndian.PutUint32(buf[0:], renameReplaceIfExists|renamePosixSemantics)
	binary.LittleEndian.PutUint32(buf[16:], uint32((len(name)-1)*2))
	for i, c := range name {
		binary.LittleEndian.PutUint16(buf[fileRenameInfoNameStart+2*i:], c)
	}
	return windows.SetFileInformationByHandle(h, fileRenameInfoEx, &buf[0], uint32(len(buf)))
}

// TestOpenSharedReadAllowsReplace pins why openSharedRead exists and what it
// can and cannot do for the writer's temp-and-rename:
//
//   - a POSIX-semantics rename succeeds while this service holds the file
//     through openSharedRead (FILE_SHARE_DELETE), and the open handle keeps
//     reading the copy it opened; through os.Open it would not;
//   - a classic MoveFileEx replace (os.Rename here, os.replace in Python)
//     fails while any handle is open, so the reader keeps its handle for one
//     read only and the writer retries on its next tick.
func TestOpenSharedReadAllowsReplace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, hailoActivityFile)
	tmp := filepath.Join(dir, "hailo.json.tmp")
	writeTmp := func(body string) {
		t.Helper()
		if err := os.WriteFile(tmp, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	f, err := openSharedRead(path)
	if err != nil {
		t.Fatalf("openSharedRead: %v", err)
	}
	writeTmp("new")
	if err := posixRenameOver(tmp, path); err != nil {
		f.Close()
		t.Fatalf("a POSIX-semantics rename failed while openSharedRead held the file: %v", err)
	}
	old, err := io.ReadAll(f)
	f.Close()
	if err != nil || string(old) != "old" {
		t.Fatalf("open handle read (%q, %v), want the copy it opened", old, err)
	}
	if got, err := readAccelActivityFile(path); err != nil || string(got) != "new" {
		t.Fatalf("next read = (%q, %v), want the replacement", got, err)
	}

	// For contrast, and logged rather than asserted because they describe the
	// platform rather than this code: the same rename under an os.Open handle
	// (no FILE_SHARE_DELETE), and a MoveFileEx replace under either handle.
	plain, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	writeTmp("newer")
	t.Logf("POSIX rename while an os.Open handle is held: %v", posixRenameOver(tmp, path))
	plain.Close()
	shared, err := openSharedRead(path)
	if err != nil {
		t.Fatal(err)
	}
	writeTmp("newest")
	t.Logf("MoveFileEx replace while an openSharedRead handle is held: %v", os.Rename(tmp, path))
	shared.Close()
	if err := os.Rename(tmp, path); err != nil {
		t.Fatalf("MoveFileEx replace with no reader handle open: %v", err)
	}
}

func TestReadAccelActivityFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := readAccelActivityFile(filepath.Join(dir, "missing.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("absent file: err = %v, want fs.ErrNotExist", err)
	}
	big := filepath.Join(dir, "big.json")
	if err := os.WriteFile(big, make([]byte, accelActivityMaxBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readAccelActivityFile(big); !errors.Is(err, errAccelActivityInvalid) {
		t.Fatalf("oversized file: err = %v, want errAccelActivityInvalid", err)
	}
}

// TestAccelActivityWriterAlive checks liveness against real processes: this
// test's own process, the same pid claimed by a writer that started before it
// existed (a reused pid), and a process that has exited.
func TestAccelActivityWriterAlive(t *testing.T) {
	self := uint32(os.Getpid())
	now := float64(time.Now().UnixMilli())
	if !accelActivityWriterAlive(accelActivity{PID: self, StartedMS: now}) {
		t.Error("the running test process reads as dead")
	}
	// A tracker that started a day before this process was created cannot
	// belong to it: the pid was reused.
	if accelActivityWriterAlive(accelActivity{PID: self, StartedMS: now - 86_400_000}) {
		t.Error("a pid created after the writer's start reads as the writer")
	}

	cmd := exec.Command("cmd.exe", "/c", "exit", "0")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run child: %v", err)
	}
	child := uint32(cmd.ProcessState.Pid())
	if accelActivityWriterAlive(accelActivity{PID: child, StartedMS: now}) {
		t.Errorf("exited child pid %d reads as alive", child)
	}
}

// fakeActivity is an injected activity source: a mutable file body, a
// settable clock and a settable liveness answer.
type fakeActivity struct {
	body  []byte
	err   error
	now   time.Time
	alive bool
}

func (f *fakeActivity) source() *hailoActivity {
	return &hailoActivity{
		read:  func() ([]byte, error) { return f.body, f.err },
		alive: func(accelActivity) bool { return f.alive },
		now:   func() time.Time { return f.now },
	}
}

// hailoWire runs one sampler tick through the collector's merge and the HTTP
// response builder and returns the Hailo row's JSON object.
func hailoWire(t *testing.T, s *hailoSampler) string {
	t.Helper()
	s.sample()
	row := hailoRow(hailoArchName(hailoArchHailo8L), s.key)
	c := &statsCollector{accels: []*hailoSampler{s}}
	snap := &statsSnapshot{}
	c.mergeAccelStats(snap)
	body := string(buildResponse([]GPUInfo{row}, nil, 0, *snap, "", nil))
	start := strings.Index(body, `"GPUs":[`)
	if start < 0 {
		t.Fatalf("no gpus array in %s", body)
	}
	end := strings.Index(body[start:], "]")
	return body[start : start+end+1]
}

func noHailoDevice() (hailoTempReader, func(), error) {
	return nil, nil, errors.New("hailo_create_device_by_id: hailo_status 74")
}

func hailoAt(temp float32) hailoOpener {
	return func() (hailoTempReader, func(), error) {
		return func() (float32, float32, uint16, error) { return temp, temp, 4, nil }, func() {}, nil
	}
}

// TestHailoRowUtilizationOnTheWire is the end-to-end rule set as a client
// sees it: which of utilization_percent and utilization_unavailable the Hailo
// row carries for each state of the activity file.
func TestHailoRowUtilizationOnTheWire(t *testing.T) {
	const key = "hailo:0000:03:00.0"
	fake := &fakeActivity{alive: true}
	s := newHailoSampler(key, "0000:03:00.0", hailoAt(51))
	s.activity = fake.source()

	// No file: unavailable, as before this source existed.
	fake.err = fs.ErrNotExist
	fake.now = at(0)
	row := hailoWire(t, s)
	if !strings.Contains(row, `"utilization_unavailable":true`) || strings.Contains(row, `"utilization_percent"`) {
		t.Fatalf("no file: row = %s, want utilization_unavailable and no utilization_percent", row)
	}
	if !strings.Contains(row, `"temperature_celsius":51`) {
		t.Fatalf("no file: the temperature was lost: %s", row)
	}

	// A fresh writer: the first sample is only a baseline...
	fake.err = nil
	fake.body = activityJSON(4242, activityEpoch.UnixMilli(), activityEpoch.UnixMilli()+5000, 1000, 1)
	fake.now = at(5200)
	if row := hailoWire(t, s); !strings.Contains(row, `"utilization_unavailable":true`) {
		t.Fatalf("baseline only: row = %s, want utilization_unavailable until a delta exists", row)
	}
	// ...and the next one is a measurement.
	fake.body = activityJSON(4242, activityEpoch.UnixMilli(), activityEpoch.UnixMilli()+10000, 3000, 1)
	fake.now = at(10200)
	row = hailoWire(t, s)
	if !strings.Contains(row, `"utilization_percent":40`) || strings.Contains(row, `"utilization_unavailable"`) {
		t.Fatalf("fresh busy writer: row = %s, want utilization_percent 40 and no utilization_unavailable", row)
	}

	// A fresh idle writer: utilization_percent is omitempty, so a measured 0
	// is absent on the wire — and so is utilization_unavailable, which is
	// what makes a client render "0%" instead of "—".
	fake.body = activityJSON(4242, activityEpoch.UnixMilli(), activityEpoch.UnixMilli()+15000, 3000, 0)
	fake.now = at(15200)
	row = hailoWire(t, s)
	if strings.Contains(row, `"utilization_unavailable"`) || strings.Contains(row, `"utilization_percent"`) {
		t.Fatalf("fresh idle writer: row = %s, want neither field (a measured 0)", row)
	}

	// The writer stops refreshing but its process lives: not reporting.
	fake.now = at(19000)
	fake.alive = true
	if row := hailoWire(t, s); !strings.Contains(row, `"utilization_unavailable":true`) {
		t.Fatalf("stale, writer alive: row = %s, want utilization_unavailable", row)
	}

	// The writer's process is gone: the device is idle, a real 0.
	fake.alive = false
	row = hailoWire(t, s)
	if strings.Contains(row, `"utilization_unavailable"`) || strings.Contains(row, `"utilization_percent"`) {
		t.Fatalf("stale, writer dead: row = %s, want a measured 0 (neither field)", row)
	}

	// The file disappears again: the reading is withdrawn, not frozen.
	fake.err = fs.ErrNotExist
	if row := hailoWire(t, s); !strings.Contains(row, `"utilization_unavailable":true`) {
		t.Fatalf("file removed: row = %s, want utilization_unavailable", row)
	}
}

// TestHailoSamplerUtilizationWithoutTemperature pins that the two sources are
// independent: a device the sampler cannot open still gets its utilization,
// and a device with neither publishes nothing at all.
func TestHailoSamplerUtilizationWithoutTemperature(t *testing.T) {
	fake := &fakeActivity{err: fs.ErrNotExist, now: at(0)}
	s := newHailoSampler("hailo:x", "x", noHailoDevice)
	s.activity = fake.source()
	s.sample()
	if _, ok := s.Latest(); ok {
		t.Fatal("no temperature and no activity published a sample")
	}

	fake.err = nil
	fake.body = activityJSON(7, activityEpoch.UnixMilli(), activityEpoch.UnixMilli()+5000, 0, 1)
	fake.now = at(5000)
	s.sample()
	fake.body = activityJSON(7, activityEpoch.UnixMilli(), activityEpoch.UnixMilli()+10000, 5000, 1)
	fake.now = at(10000)
	s.sample()
	st, ok := s.Latest()
	if !ok || !st.UtilizationKnown || st.UtilizationPct != 100 || st.TemperatureC != 0 {
		t.Fatalf("Latest() = (%+v, %v), want 100 %% known and no temperature", st, ok)
	}
}

// TestHailoActivityRealFile drives the live source (newHailoActivity) against
// a real file in NVPAIR_ACCEL_ACTIVITY_DIR, written by temp-and-rename the way
// the writer does, with this test's own process as the writer.
func TestHailoActivityRealFile(t *testing.T) {
	dir := t.TempDir()
	a := newHailoActivity(func(k string) string {
		if k == accelActivityDirEnv {
			return dir
		}
		return ""
	})
	path := filepath.Join(dir, hailoActivityFile)
	write := func(body []byte) {
		t.Helper()
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, body, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, path); err != nil {
			t.Fatal(err)
		}
	}

	if _, known := a.sample("x"); known {
		t.Fatal("absent file produced a figure")
	}

	// The writer's tracker starts after its process does, so this process
	// qualifies as the writer of a tracker started now.
	pid := os.Getpid()
	started := time.Now().UnixMilli()
	now := started + 2000
	// Pin the clock so the two writes are 1 s apart on the writer's clock
	// and both fresh on the reader's.
	a.now = func() time.Time { return time.UnixMilli(now) }
	write(activityJSON(pid, started, now-1000, 20_000, 1))
	if _, known := a.sample("x"); known {
		t.Fatal("baseline produced a figure")
	}
	write(append([]byte("\xef\xbb\xbf"), activityJSON(pid, started, now, 20_750, 1)...))
	if pct, known := a.sample("x"); !known || pct != 75 {
		t.Fatalf("second sample = (%d, %v), want 75 known", pct, known)
	}

	// The same file read 10 s later: stale, and this process — the writer —
	// is alive, so nothing is known.
	a.now = func() time.Time { return time.UnixMilli(now + 10_000) }
	if _, known := a.sample("x"); known {
		t.Fatal("a stale file from a live writer produced a figure")
	}
}
