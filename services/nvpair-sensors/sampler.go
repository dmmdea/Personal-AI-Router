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
	// power returns the average package power in whole watts since the
	// previous call, and false when there is none to give — no energy
	// counter on this part, the first sample after an open, or a read that
	// failed.
	//
	// It returns no error on purpose. Power is the secondary reading here;
	// surfacing its failures as sensor faults would trip the sampler's
	// reopen path and cost the host the temperature it came for.
	power(now time.Time) (float64, bool)
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

// sampler owns the sensors and publishes the latest report for the pipe
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
	// powerAnnounced keeps the "power readable" line to one per open. A part
	// with no energy counter never logs it at all; intel_windows.go says why
	// once, at open.
	powerAnnounced bool
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
// publish one report — and returns how long to wait before the next one.
func (s *sampler) tick(st *samplerState) time.Duration {
	cpu, cpuErr, wait := s.tickCPU(st)
	r := &hostsensors.Report{HelperVersion: Version, CPU: cpu}
	if cpu == nil {
		r.Error = cpuErr
	}
	s.latest.Store(r)
	return wait
}

// tickCPU samples the package sensor. It returns the reading (nil when there
// is none), the reason there is none, and how long to wait before the next
// tick.
func (s *sampler) tickCPU(st *samplerState) (*hostsensors.CPUReading, string, time.Duration) {
	if st.sensor == nil {
		opened, err := s.open()
		if err != nil {
			return nil, s.noteCPUError(err.Error(), st), s.retry
		}
		st.sensor = opened
		st.failures = 0
		st.powerAnnounced = false
		s.log.Info("CPU package sensor open", "source", sourceIntelMSR, "tjmax_celsius", st.sensor.tjMax())
	}
	c, err := st.sensor.read()
	if err != nil {
		msg := s.noteCPUError(err.Error(), st)
		st.failures++
		if st.failures >= sensorReopenAfterFailures {
			// The executor is presumed gone (driver restarted, module
			// unloaded); drop it and go back through open.
			st.sensor.close()
			st.sensor = nil
			return nil, msg, s.retry
		}
		return nil, msg, s.interval
	}
	st.failures = 0
	if st.lastErr != "" {
		s.log.Info("CPU package sensor readable again")
		st.lastErr = ""
	}
	now := s.now()
	reading := &hostsensors.CPUReading{
		PackageCelsius: c,
		TjMaxCelsius:   st.sensor.tjMax(),
		Source:         sourceIntelMSR,
		SampledAt:      now.UTC(),
	}
	// Power is sampled from the same open sensor, on the same tick, and
	// stamped with the same SampledAt — so a reader applying one freshness
	// window gets both readings or neither, and never a wattage from one
	// tick beside a temperature from the next. A sample with no figure yet
	// (the first after an open) simply leaves the field at zero, which the
	// omitempty tag drops.
	if watts, ok := st.sensor.power(now); ok {
		reading.PackageWatts = watts
		if !st.powerAnnounced {
			s.log.Info("CPU package power readable", "watts", watts)
			st.powerAnnounced = true
		}
	}
	return reading, "", s.interval
}

// noteCPUError logs the reason there is no CPU reading once per distinct
// reason rather than once per tick, and returns it for the report.
func (s *sampler) noteCPUError(msg string, st *samplerState) string {
	if st.lastErr != msg {
		s.log.Warn("CPU package sensor unavailable", "err", msg)
		st.lastErr = msg
	}
	return msg
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
