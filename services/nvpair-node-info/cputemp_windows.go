// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"nvpair-shared/hostsensors"
)

// Windows CPU package temperature.
//
// No unprivileged Windows API reports it: the sensor is a model-specific
// register that only ring 0 can read, and the signed PawnIO driver that
// exposes it admits administrators only, while this service runs
// unprivileged under the desktop app. The read therefore lives in the
// nvpair-sensors service, which answers each connection to its named pipe
// with one JSON report (nvpair-shared/hostsensors). This poller asks it on the
// same cadence as the nvidia-smi GPU poller and publishes the reading for the
// 1 s PDH tick to fold into the snapshot.
//
// A host without the helper reports no CPU temperature, exactly as before:
// the poller logs that once, checks again every 30 s, and never fabricates a
// number. A report older than cpuTempMaxAge is dropped rather than repeated.

const (
	cpuTempPollInterval  = 5 * time.Second
	cpuTempRetryInterval = 30 * time.Second
	cpuTempDialTimeout   = 500 * time.Millisecond
	cpuTempMaxAge        = 30 * time.Second
)

// cpuSample is the pair of CPU readings one helper report carries. They are
// published behind a single atomic pointer so a reader can never combine a
// temperature from one poll with a wattage from another — the helper samples
// both off the same open sensor on the same tick, and the collector should
// not undo that.
type cpuSample struct {
	celsius uint32
	watts   float64
}

// cpuTempPoller owns the pipe reads and publishes those readings atomically.
type cpuTempPoller struct {
	read func() (hostsensors.Report, error)
	// observe, when set, is handed every report this poller fetches,
	// including the failures. One report carries every host sensor the
	// helper reads, so the board row (board_windows.go) rides along on
	// these reads instead of opening a second connection on its own timer.
	// Called from the poll goroutine, which is its only caller.
	observe func(hostsensors.Report, error)
	now     func() time.Time
	latest  atomic.Pointer[cpuSample]

	// announced / lastNote belong to the poll goroutine: they keep the log
	// to one line per state change instead of one per tick.
	announced bool
	lastNote  string

	stop chan struct{}
	done chan struct{}
}

func startCPUTempPoller(observe func(hostsensors.Report, error)) *cpuTempPoller {
	p := newCPUTempPoller(func() (hostsensors.Report, error) {
		return hostsensors.Read(cpuTempDialTimeout)
	}, time.Now)
	p.observe = observe
	go p.run()
	return p
}

func newCPUTempPoller(read func() (hostsensors.Report, error), now func() time.Time) *cpuTempPoller {
	return &cpuTempPoller{
		read: read,
		now:  now,
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
}

func (p *cpuTempPoller) run() {
	defer close(p.done)
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-timer.C:
		}
		if p.poll() {
			timer.Reset(cpuTempPollInterval)
		} else {
			timer.Reset(cpuTempRetryInterval)
		}
	}
}

// poll reads one report and publishes the readings it carries, or zeroes.
// It returns whether the helper answered at all, which picks the next delay.
//
// Temperature and power are asked for separately against the same freshness
// window: a helper on a part whose energy counter it could not resolve sends
// a temperature and no wattage, and a reading is not worth dropping because
// its companion is absent.
func (p *cpuTempPoller) poll() bool {
	r, err := p.read()
	if p.observe != nil {
		p.observe(r, err)
	}
	if err != nil {
		p.latest.Store(&cpuSample{})
		p.announced = false
		p.note(slog.LevelInfo, "unreachable: "+err.Error(),
			"host sensor helper not reachable; cpu.temperature_celsius will be omitted",
			"pipe", hostsensors.PipeName, "err", err)
		return false
	}
	now := p.now()
	celsius, haveTemp := r.CPUPackage(now, cpuTempMaxAge)
	watts, _ := r.CPUPackageWatts(now, cpuTempMaxAge)
	p.latest.Store(&cpuSample{celsius: celsius, watts: watts})
	if haveTemp {
		if !p.announced {
			slog.Info("CPU temperature source", "helper", "nvpair-sensors",
				"helper_version", r.HelperVersion, "source", r.CPU.Source, "package_watts", watts)
			p.announced = true
			p.lastNote = ""
		}
		return true
	}
	p.announced = false
	reason := r.Error
	if reason == "" {
		reason = "the report carries no fresh CPU reading"
	}
	p.note(slog.LevelWarn, "no reading: "+reason,
		"host sensor helper has no CPU package reading; cpu.temperature_celsius omitted",
		"reason", reason)
	return true
}

// note logs msg once per distinct key, so a steady failure costs one line.
func (p *cpuTempPoller) note(level slog.Level, key, msg string, args ...any) {
	if p.lastNote == key {
		return
	}
	p.lastNote = key
	slog.Log(context.Background(), level, msg, args...)
}

// current returns the latest package temperature, zero when unknown. Safe
// on a nil poller and before the first poll.
func (p *cpuTempPoller) current() uint32 {
	if s := p.sample(); s != nil {
		return s.celsius
	}
	return 0
}

// currentPower returns the latest package power draw in whole watts, zero
// when unknown. Safe on a nil poller and before the first poll.
func (p *cpuTempPoller) currentPower() float64 {
	if s := p.sample(); s != nil {
		return s.watts
	}
	return 0
}

func (p *cpuTempPoller) sample() *cpuSample {
	if p == nil {
		return nil
	}
	return p.latest.Load()
}

func (p *cpuTempPoller) Stop() {
	if p == nil {
		return
	}
	close(p.stop)
	<-p.done
}
