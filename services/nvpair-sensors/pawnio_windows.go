// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// PawnIO (https://pawnio.eu, namazso) is the signed ring-0 executor that
// LibreHardwareMonitor, HWiNFO and vendor dashboards use: user mode loads a
// signed Pawn module into the driver and calls the module's exported ioctl_*
// functions. The driver checks the module signature, and each module
// whitelists the registers it will touch — IntelMSR.bin exposes only the
// thermal, power and frequency MSRs. The device admits administrators and
// SYSTEM only, which is why this helper runs as a service.
//
// The documented entry points live in PawnIOLib.dll, installed next to the
// driver. They are called through that library rather than by reproducing its
// IOCTL layout, so a driver update cannot silently break a read.

const (
	pawnIOUninstallKey = `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\PawnIO`
	pawnIOLibName      = "PawnIOLib.dll"

	hrAccessDenied = 0x80070005 // E_ACCESSDENIED: not elevated
	hrFileNotFound = 0x80070002 // the device does not exist: driver not running
)

var (
	errPawnIONotInstalled = errors.New("PawnIO is not installed (https://pawnio.eu), so there is no CPU package sensor to read")
	errPawnIONotRunning   = errors.New("the PawnIO device is absent: the driver is installed but not running")
	errPawnIOAccessDenied = errors.New("PawnIO refused the open: its device admits administrators and SYSTEM only")
)

// pawnIOLibraryPath locates PawnIOLib.dll: the install location PawnIO's
// uninstall entry records, else the default Program Files directory. Only a
// full path is ever loaded, so the DLL search order cannot be hijacked.
func pawnIOLibraryPath() (string, error) {
	var candidates []string
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, pawnIOUninstallKey, registry.QUERY_VALUE|registry.WOW64_64KEY); err == nil {
		if loc, _, err := k.GetStringValue("InstallLocation"); err == nil && loc != "" {
			candidates = append(candidates, filepath.Join(loc, pawnIOLibName))
		}
		k.Close()
	}
	if pf := os.Getenv("ProgramFiles"); pf != "" {
		candidates = append(candidates, filepath.Join(pf, "PawnIO", pawnIOLibName))
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c, nil
		}
	}
	return "", errPawnIONotInstalled
}

// pawnIO is one open executor with one loaded module.
type pawnIO struct {
	dll     *windows.DLL
	loadFn  *windows.Proc
	execFn  *windows.Proc
	closeFn *windows.Proc
	handle  uintptr
}

// hresult turns a PawnIOLib return value into an error.
func hresult(call string, r uintptr) error {
	hr := uint32(r)
	switch hr {
	case 0:
		return nil
	case hrAccessDenied:
		return errPawnIOAccessDenied
	case hrFileNotFound:
		return errPawnIONotRunning
	}
	return fmt.Errorf("%s: HRESULT 0x%08X", call, hr)
}

// openPawnIO loads PawnIOLib.dll and opens an executor.
func openPawnIO() (*pawnIO, error) {
	path, err := pawnIOLibraryPath()
	if err != nil {
		return nil, err
	}
	dll, err := windows.LoadDLL(path)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", path, err)
	}
	p := &pawnIO{dll: dll}
	var openFn *windows.Proc
	for name, dst := range map[string]**windows.Proc{
		"pawnio_open":    &openFn,
		"pawnio_load":    &p.loadFn,
		"pawnio_execute": &p.execFn,
		"pawnio_close":   &p.closeFn,
	} {
		proc, err := dll.FindProc(name)
		if err != nil {
			dll.Release()
			return nil, fmt.Errorf("%s lacks %s: %w", path, name, err)
		}
		*dst = proc
	}
	r, _, _ := openFn.Call(uintptr(unsafe.Pointer(&p.handle)))
	if err := hresult("pawnio_open", r); err != nil {
		dll.Release()
		return nil, err
	}
	return p, nil
}

// load hands a signed module blob to the driver.
func (p *pawnIO) load(blob []byte) error {
	if len(blob) == 0 {
		return errors.New("pawnio_load: empty module")
	}
	r, _, _ := p.loadFn.Call(p.handle, uintptr(unsafe.Pointer(&blob[0])), uintptr(len(blob)))
	return hresult("pawnio_load", r)
}

// call runs one exported function of the loaded module. in and out are the
// module's cell arrays (64-bit each); the result is truncated to the count
// the module reported writing.
func (p *pawnIO) call(name string, in []uint64, outLen int) ([]uint64, error) {
	cname, err := windows.BytePtrFromString(name)
	if err != nil {
		return nil, err
	}
	if outLen < 1 {
		outLen = 1
	}
	out := make([]uint64, outLen)
	var inPtr unsafe.Pointer
	if len(in) > 0 {
		inPtr = unsafe.Pointer(&in[0])
	}
	var returned uintptr
	r, _, _ := p.execFn.Call(
		p.handle,
		uintptr(unsafe.Pointer(cname)),
		uintptr(inPtr),
		uintptr(len(in)),
		uintptr(unsafe.Pointer(&out[0])),
		uintptr(outLen),
		uintptr(unsafe.Pointer(&returned)),
	)
	if err := hresult("pawnio_execute("+name+")", r); err != nil {
		return nil, err
	}
	if int(returned) < outLen {
		out = out[:returned]
	}
	return out, nil
}

// readMSR reads one model-specific register through the module's
// ioctl_read_msr (in[0] = MSR index, out[0] = value). The module refuses any
// register outside its allow list.
func (p *pawnIO) readMSR(msr uint32) (uint64, error) {
	out, err := p.call("ioctl_read_msr", []uint64{uint64(msr)}, 1)
	if err != nil {
		return 0, fmt.Errorf("read MSR 0x%X: %w", msr, err)
	}
	if len(out) < 1 {
		return 0, fmt.Errorf("read MSR 0x%X: the module returned no value", msr)
	}
	return out[0], nil
}

// close releases the executor (unloading the module) and the library.
func (p *pawnIO) close() {
	if p == nil {
		return
	}
	if p.handle != 0 {
		p.closeFn.Call(p.handle)
		p.handle = 0
	}
	if p.dll != nil {
		p.dll.Release()
		p.dll = nil
	}
}
