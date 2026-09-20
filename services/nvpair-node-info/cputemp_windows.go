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

// cpuTempPoller owns the pipe reads and publishes degrees atomically.
type cpuTempPoller struct {
	read   func() (hostsensors.Report, error)
	now    func() time.Time
	latest atomic.Uint32

	// announced / lastNote belong to the poll goroutine: they keep the log
	// to one line per state change instead of one per tick.
	announced bool
	lastNote  string

	stop chan struct{}
	done chan struct{}
}

func startCPUTempPoller() *cpuTempPoller {
	p := newCPUTempPoller(func() (hostsensors.Report, error) {
		return hostsensors.Read(cpuTempDialTimeout)
	}, time.Now)
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

// poll reads one report and publishes the temperature it carries, or zero.
// It returns whether the helper answered at all, which picks the next delay.
func (p *cpuTempPoller) poll() bool {
	r, err := p.read()
	if err != nil {
		p.latest.Store(0)
		p.announced = false
		p.note(slog.LevelInfo, "unreachable: "+err.Error(),
			"host sensor helper not reachable; cpu.temperature_celsius will be omitted",
			"pipe", hostsensors.PipeName, "err", err)
		return false
	}
	if c, ok := r.CPUPackage(p.now(), cpuTempMaxAge); ok {
		p.latest.Store(c)
		if !p.announced {
			slog.Info("CPU temperature source", "helper", "nvpair-sensors", "helper_version", r.HelperVersion, "source", r.CPU.Source)
			p.announced = true
			p.lastNote = ""
		}
		return true
	}
	p.latest.Store(0)
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
// on a nil poller.
func (p *cpuTempPoller) current() uint32 {
	if p == nil {
		return 0
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
