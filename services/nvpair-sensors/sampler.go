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

// boardSensor is one open Super I/O chip: the motherboard's own temperature,
// fan and voltage inputs. The Windows implementation is in board_windows.go.
//
// It is a second, independent sensor rather than part of packageSensor
// because the two fail apart: a host can have a readable CPU package sensor
// and an unsupported board chip, or the reverse, and neither absence may take
// the other's reading down with it.
type boardSensor interface {
	// read takes one full sample of the board's sensors. SampledAt is left
	// for the sampler to stamp, so both sections of a report agree on when
	// the tick happened.
	read() (hostsensors.BoardReading, error)
	// openNote is a non-fatal condition worth one log line at open, empty
	// when the sensor opened cleanly.
	openNote() string
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
	open func() (packageSensor, error)
	// openBoard is the board sensor's open hook, nil on a build or a test
	// that has none — the report then simply carries no board section.
	openBoard func() (boardSensor, error)
	now       func() time.Time
	interval  time.Duration
	retry     time.Duration
	log       *slog.Logger
	latest    atomic.Pointer[hostsensors.Report]
	stop      chan struct{}
	done      chan struct{}
}

// samplerState is what one tick carries to the next: the open sensors (nil
// while unavailable), the last reason logged, and the failure streaks. The
// CPU and the board each keep their own set, so one being unreadable never
// costs the other its reading.
type samplerState struct {
	sensor   packageSensor
	lastErr  string
	failures int

	board         boardSensor
	boardLastErr  string
	boardFailures int
	// boardRetryAt is when the next open attempt is allowed while the board
	// sensor is unavailable. Retrying every tick would hammer the driver on
	// a host that has no supported chip at all.
	boardRetryAt time.Time
}

func startSampler(interval time.Duration, log *slog.Logger, open func() (packageSensor, error), openBoard func() (boardSensor, error)) *sampler {
	s := newSampler(interval, sensorRetryInterval, log, open, time.Now)
	s.openBoard = openBoard
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
		if st.board != nil {
			st.board.close()
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

// tick performs one sampling step — open each sensor if needed, read them,
// publish one report — and returns how long to wait before the next one.
//
// The CPU sensor sets the cadence, because it is the reading every consumer
// depends on; the board is sampled alongside it and never changes the delay.
func (s *sampler) tick(st *samplerState) time.Duration {
	cpu, cpuErr, wait := s.tickCPU(st)
	board := s.tickBoard(st)
	r := &hostsensors.Report{HelperVersion: Version, CPU: cpu, Board: board}
	if cpu == nil {
		// Error has always described the CPU sensor, which is the reading a
		// reader gates on; an absent board section explains itself in the
		// log rather than overwriting this.
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
	return &hostsensors.CPUReading{
		PackageCelsius: c,
		TjMaxCelsius:   st.sensor.tjMax(),
		Source:         sourceIntelMSR,
		SampledAt:      s.now().UTC(),
	}, "", s.interval
}

// tickBoard samples the Super I/O chip, or returns nil when the host has
// none this build reads. A board that has never opened is retried on the
// same slow cadence the CPU sensor uses, not on every tick.
func (s *sampler) tickBoard(st *samplerState) *hostsensors.BoardReading {
	if s.openBoard == nil {
		return nil
	}
	now := s.now()
	if st.board == nil {
		if now.Before(st.boardRetryAt) {
			return nil
		}
		opened, err := s.openBoard()
		if err != nil {
			st.boardRetryAt = now.Add(s.retry)
			s.noteBoardError(err.Error(), st)
			return nil
		}
		st.board = opened
		st.boardFailures = 0
		if note := opened.openNote(); note != "" {
			s.log.Warn("board sensor open with a caveat", "note", note)
		}
	}
	reading, err := st.board.read()
	if err != nil {
		s.noteBoardError(err.Error(), st)
		st.boardFailures++
		if st.boardFailures >= sensorReopenAfterFailures {
			st.board.close()
			st.board = nil
			st.boardRetryAt = now.Add(s.retry)
		}
		return nil
	}
	if st.boardLastErr != "" {
		s.log.Info("board sensor readable again", "chip", reading.Chip)
		st.boardLastErr = ""
	}
	st.boardFailures = 0
	reading.SampledAt = now.UTC()
	return &reading
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

// noteBoardError does the same for the board. It is logged at Info, not
// Warn: a host whose Super I/O chip this build does not decode is an
// ordinary, permanent state, not a fault.
func (s *sampler) noteBoardError(msg string, st *samplerState) {
	if st.boardLastErr == msg {
		return
	}
	st.boardLastErr = msg
	s.log.Info("no motherboard sensor readings", "reason", msg)
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
