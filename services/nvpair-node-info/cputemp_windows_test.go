// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"errors"
	"testing"
	"time"

	"nvpair-shared/hostsensors"
)

// TestCPUTempPollerPublishesOnlyFreshReadings pins the contract with the
// helper: a fresh reading is published, a stale one, a helper without a
// sensor and an unreachable helper all publish zero (omitted from JSON), and
// only an unreachable helper slows the poll to the retry cadence.
func TestCPUTempPollerPublishesOnlyFreshReadings(t *testing.T) {
	now := time.Date(2026, 9, 19, 22, 0, 0, 0, time.UTC)
	var rep hostsensors.Report
	var readErr error
	p := newCPUTempPoller(func() (hostsensors.Report, error) { return rep, readErr }, func() time.Time { return now })

	rep = hostsensors.Report{Schema: hostsensors.Schema, HelperVersion: "0.1.0", CPU: &hostsensors.CPUReading{
		PackageCelsius: 58, TjMaxCelsius: 110, Source: "intel-msr", SampledAt: now.Add(-2 * time.Second),
	}}
	if up := p.poll(); !up || p.current() != 58 {
		t.Fatalf("fresh reading: up=%v current=%d, want up=true current=58", up, p.current())
	}

	rep.CPU.SampledAt = now.Add(-cpuTempMaxAge - time.Second)
	if up := p.poll(); !up || p.current() != 0 {
		t.Fatalf("stale reading: up=%v current=%d, want up=true current=0", up, p.current())
	}

	rep = hostsensors.Report{Schema: hostsensors.Schema, HelperVersion: "0.1.0", Error: "PawnIO is not installed"}
	if up := p.poll(); !up || p.current() != 0 {
		t.Fatalf("helper without sensor: up=%v current=%d, want up=true current=0", up, p.current())
	}

	rep = hostsensors.Report{}
	readErr = errors.New("open \\\\.\\pipe\\nvpair-sensors: The system cannot find the file specified.")
	if up := p.poll(); up || p.current() != 0 {
		t.Fatalf("unreachable helper: up=%v current=%d, want up=false current=0", up, p.current())
	}

	readErr = nil
	rep = hostsensors.Report{Schema: hostsensors.Schema, CPU: &hostsensors.CPUReading{PackageCelsius: 61, SampledAt: now}}
	if up := p.poll(); !up || p.current() != 61 {
		t.Fatalf("recovered: up=%v current=%d, want up=true current=61", up, p.current())
	}
}

// TestCPUTempPollerPublishesPackageWatts pins the second reading the helper
// now carries. The cases that matter are the ones where the two readings do
// NOT travel together: a part whose energy counter the helper could not
// resolve still publishes its temperature, and a stale report publishes
// neither.
func TestCPUTempPollerPublishesPackageWatts(t *testing.T) {
	now := time.Date(2026, 9, 20, 16, 0, 0, 0, time.UTC)
	var rep hostsensors.Report
	p := newCPUTempPoller(func() (hostsensors.Report, error) { return rep, nil }, func() time.Time { return now })

	rep = hostsensors.Report{Schema: hostsensors.Schema, HelperVersion: "0.2.0", CPU: &hostsensors.CPUReading{
		PackageCelsius: 55, PackageWatts: 140, TjMaxCelsius: 100, Source: "intel-msr", SampledAt: now.Add(-time.Second),
	}}
	if up := p.poll(); !up || p.current() != 55 || p.currentPower() != 140 {
		t.Fatalf("fresh reading: up=%v temp=%d watts=%v, want true/55/140", up, p.current(), p.currentPower())
	}

	// No energy counter on this part: the temperature must survive on its own.
	rep.CPU.PackageWatts = 0
	if up := p.poll(); !up || p.current() != 55 || p.currentPower() != 0 {
		t.Fatalf("temperature only: up=%v temp=%d watts=%v, want true/55/0", up, p.current(), p.currentPower())
	}

	// Stale: both readings drop, because the whole report is one sample.
	rep.CPU.PackageWatts = 140
	rep.CPU.SampledAt = now.Add(-cpuTempMaxAge - time.Second)
	if up := p.poll(); !up || p.current() != 0 || p.currentPower() != 0 {
		t.Fatalf("stale: up=%v temp=%d watts=%v, want true/0/0", up, p.current(), p.currentPower())
	}
}

func TestCPUTempPollerNilIsSafe(t *testing.T) {
	var p *cpuTempPoller
	if p.current() != 0 {
		t.Fatal("nil poller must report no temperature")
	}
	if p.currentPower() != 0 {
		t.Fatal("nil poller must report no power")
	}
	// Before the first poll the pointer is nil too, which must read the
	// same way rather than panic.
	fresh := newCPUTempPoller(func() (hostsensors.Report, error) { return hostsensors.Report{}, nil }, time.Now)
	if fresh.current() != 0 || fresh.currentPower() != 0 {
		t.Fatal("a poller that has not polled yet must report nothing")
	}
	p.Stop()
}

// TestCPUTempPollerStopsPromptly guards the shutdown path: Stop returns even
// while the poller is waiting out a retry delay.
func TestCPUTempPollerStopsPromptly(t *testing.T) {
	p := newCPUTempPoller(func() (hostsensors.Report, error) { return hostsensors.Report{}, errors.New("down") }, time.Now)
	go p.run()
	done := make(chan struct{})
	go func() { p.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return")
	}
}
