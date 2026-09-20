// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"log/slog"
	"sync/atomic"
	"time"

	"nvpair-shared/hostsensors"
)

// packageSensor is one open CPU package sensor. The Intel implementation is
// in intel_windows.go; the sampler only needs these three calls, which keeps
// its open / read / reopen state machine testable on every platform.
type packageSensor interface {
	// read returns the current package temperature in whole degrees.
	read() (uint32, error)
	// tjMax is the junction maximum the readouts are relative to.
	tjMax() uint32
	close()
}

const (
	// sensorRetryInterval is how often the sampler retries opening the sensor
	// while it is unavailable — PawnIO may be installed after the service.
	sensorRetryInterval = 30 * time.Second

	// sensorReopenAfterFailures is the number of consecutive read failures
	// after which the executor is assumed dead (a driver restart drops it)
	// and is closed and reopened, rather than retried forever. A single
	// invalid readout is transient and does not cost a reopen.
	sensorReopenAfterFailures = 3
)

// sampler owns the sensor and publishes the latest report for the pipe
// server to hand out. One goroutine; readers take an atomic pointer.
type sampler struct {
	open     func() (packageSensor, error)
	now      func() time.Time
	interval time.Duration
	retry    time.Duration
	log      *slog.Logger
	latest   atomic.Pointer[hostsensors.Report]
	stop     chan struct{}
	done     chan struct{}
}

// samplerState is what one tick carries to the next: the open sensor (nil
// while unavailable), the last reason logged, and the failure streak.
type samplerState struct {
	sensor   packageSensor
	lastErr  string
	failures int
}

func startSampler(interval time.Duration, log *slog.Logger, open func() (packageSensor, error)) *sampler {
	s := newSampler(interval, sensorRetryInterval, log, open, time.Now)
	go s.run()
	return s
}

func newSampler(interval, retry time.Duration, log *slog.Logger, open func() (packageSensor, error), now func() time.Time) *sampler {
	s := &sampler{
		open:     open,
		now:      now,
		interval: interval,
		retry:    retry,
		log:      log,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	s.latest.Store(&hostsensors.Report{HelperVersion: Version, Error: "starting"})
	return s
}

func (s *sampler) run() {
	defer close(s.done)
	st := &samplerState{}
	defer func() {
		if st.sensor != nil {
			st.sensor.close()
		}
	}()
	next := time.NewTimer(0)
	defer next.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-next.C:
		}
		next.Reset(s.tick(st))
	}
}

// tick performs one sampling step — open the sensor if needed, read it,
// publish — and returns how long to wait before the next one.
func (s *sampler) tick(st *samplerState) time.Duration {
	if st.sensor == nil {
		opened, err := s.open()
		if err != nil {
			s.publishError(err.Error(), st)
			return s.retry
		}
		st.sensor = opened
		st.failures = 0
		s.log.Info("CPU package sensor open", "source", sourceIntelMSR, "tjmax_celsius", st.sensor.tjMax())
	}
	c, err := st.sensor.read()
	if err != nil {
		s.publishError(err.Error(), st)
		st.failures++
		if st.failures >= sensorReopenAfterFailures {
			// The executor is presumed gone (driver restarted, module
			// unloaded); drop it and go back through open.
			st.sensor.close()
			st.sensor = nil
			return s.retry
		}
		return s.interval
	}
	st.failures = 0
	if st.lastErr != "" {
		s.log.Info("CPU package sensor readable again")
		st.lastErr = ""
	}
	s.latest.Store(&hostsensors.Report{
		HelperVersion: Version,
		CPU: &hostsensors.CPUReading{
			PackageCelsius: c,
			TjMaxCelsius:   st.sensor.tjMax(),
			Source:         sourceIntelMSR,
			SampledAt:      s.now().UTC(),
		},
	})
	return s.interval
}

// publishError replaces the report with the reason there is no reading,
// logging it once per distinct reason rather than per tick.
func (s *sampler) publishError(msg string, st *samplerState) {
	if st.lastErr != msg {
		s.log.Warn("CPU package sensor unavailable", "err", msg)
		st.lastErr = msg
	}
	s.latest.Store(&hostsensors.Report{HelperVersion: Version, Error: msg})
}

// report returns the latest published report by value.
func (s *sampler) report() hostsensors.Report {
	return *s.latest.Load()
}

// Stop ends sampling and releases the sensor.
func (s *sampler) Stop() {
	close(s.stop)
	<-s.done
}
