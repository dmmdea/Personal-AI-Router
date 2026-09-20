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

func TestCPUTempPollerNilIsSafe(t *testing.T) {
	var p *cpuTempPoller
	if p.current() != 0 {
		t.Fatal("nil poller must report no temperature")
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
