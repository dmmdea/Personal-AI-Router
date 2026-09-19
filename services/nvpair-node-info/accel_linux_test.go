// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"testing"

	"nvpair-shared/noderec"
)

// TestParseInterruptCounts pins the gasket interrupt_counts parse. The busy
// signal is "the sum moved between two polls", so a regression that drops a
// vector or reads a label as a count would either hide activity or report a
// permanently busy device.
func TestParseInterruptCounts(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantSum uint64
		wantOK  bool
	}{
		{
			name:    "idle device as gasket prints it (one vector per line)",
			in:      "0x00: 0\n0x01: 0\n0x02: 0\n0x03: 0\n0x04: 0\n0x05: 0\n0x06: 0\n0x07: 0\n0x08: 0\n0x09: 0\n0x0a: 0\n0x0b: 0\n0x0c: 0\n",
			wantSum: 0,
			wantOK:  true,
		},
		{
			name:    "counts on several vectors are summed",
			in:      "0x00: 12 0x01: 0 0x02: 30 0x03: 1\n",
			wantSum: 43,
			wantOK:  true,
		},
		{
			name:   "empty file",
			in:     "",
			wantOK: false,
		},
		{
			name:   "labels only, no counts",
			in:     "0x00: 0x01:",
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sum, ok := parseInterruptCounts(tc.in)
			if ok != tc.wantOK || sum != tc.wantSum {
				t.Fatalf("parseInterruptCounts(%q) = (%d, %v), want (%d, %v)", tc.in, sum, ok, tc.wantSum, tc.wantOK)
			}
		})
	}
}

func TestParseMillidegrees(t *testing.T) {
	cases := []struct {
		in     string
		want   uint32
		wantOK bool
	}{
		{"51550\n", 52, true},
		{"51449", 51, true},
		{"0", 0, true},
		{"-5000", 0, false},
		{"", 0, false},
		{"warm", 0, false},
	}
	for _, tc := range cases {
		got, ok := parseMillidegrees(tc.in)
		if ok != tc.wantOK || got != tc.want {
			t.Errorf("parseMillidegrees(%q) = (%d, %v), want (%d, %v)", tc.in, got, ok, tc.want, tc.wantOK)
		}
	}
}

func TestApexProductName(t *testing.T) {
	cases := []struct {
		vendor, device, want string
	}{
		{"0x1ac1\n", "0x089a\n", "Google Coral Edge TPU"},
		{"0x1AC1", "0x089A", "Google Coral Edge TPU"},
		{"0x1234", "0x5678", "Edge TPU (apex 1234:5678)"},
		{"", "", "Edge TPU (apex)"},
	}
	for _, tc := range cases {
		if got := apexProductName(tc.vendor, tc.device); got != tc.want {
			t.Errorf("apexProductName(%q, %q) = %q, want %q", tc.vendor, tc.device, got, tc.want)
		}
	}
}

// TestAccelSamplerObserve drives the busy window with a synthetic counter
// sequence: the first reading only primes the baseline, a failed read is
// skipped (not recorded as idle), and the percentage is the busy fraction of
// the observed sub-intervals.
func TestAccelSamplerObserve(t *testing.T) {
	s := &accelSampler{}
	if got := s.stat().UtilizationPct; got != 0 {
		t.Fatalf("fresh sampler utilization = %d, want 0", got)
	}

	s.observe(100, true) // prime
	if s.window.filled != 0 {
		t.Fatalf("priming read filled the window: %d", s.window.filled)
	}

	s.observe(100, true) // idle
	s.observe(100, true) // idle
	s.observe(105, true) // busy
	s.observe(0, false)  // read failure: skipped
	s.observe(105, true) // idle
	if got := s.stat().UtilizationPct; got != 25 {
		t.Fatalf("after 4 observations (1 busy) utilization = %d, want 25", got)
	}

	// Fill the ring past its capacity; only the last accelWindow flags count.
	for i := 0; i < accelWindow; i++ {
		s.observe(uint64(200+i), true) // every one moves: busy
	}
	if got := s.stat().UtilizationPct; got != 100 {
		t.Fatalf("after %d consecutive busy observations utilization = %d, want 100", accelWindow, got)
	}
	for i := 0; i < accelWindow/2; i++ {
		s.observe(s.prev, true) // counter static: idle
	}
	if got := s.stat().UtilizationPct; got != 50 {
		t.Fatalf("after half the window went idle utilization = %d, want 50", got)
	}
}

// TestAccelSamplerCounterReset pins that a counter going backwards (driver
// reload) still reads as activity rather than panicking on an unsigned delta.
func TestAccelSamplerCounterReset(t *testing.T) {
	s := &accelSampler{}
	s.observe(500, true)
	s.observe(3, true)
	if got := s.stat().UtilizationPct; got != 100 {
		t.Fatalf("counter reset utilization = %d, want 100", got)
	}
}

func TestDetectAcceleratorsRowShape(t *testing.T) {
	row := GPUInfo{
		Name:     apexProductName("0x1ac1", "0x089a"),
		Kind:     noderec.GPUKindAccelerator,
		statsKey: accelStatsKey("apex_0"),
	}
	if row.statsKey != "apex:apex_0" {
		t.Fatalf("statsKey = %q", row.statsKey)
	}
	if noderec.MaxGPUUtilization([]noderec.GPUInfo{{Kind: row.Kind, UtilizationPercent: 100}}) != 0 {
		t.Fatal("an accelerator row must not contribute to GPU pressure")
	}
}
