// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"log/slog"
	"sync/atomic"
	"time"

	"nvpair-shared/hostsensors"
)

// sensorRetryInterval is how often the sampler retries opening the sensor
// while it is unavailable — PawnIO may be installed after the service, and a
// driver restart drops the executor.
const sensorRetryInterval = 30 * time.Second

// sampler owns the sensor and publishes the latest report for the pipe
// server to hand out. One goroutine; readers take an atomic pointer.
type sampler struct {
	interval time.Duration
	log      *slog.Logger
	latest   atomic.Pointer[hostsensors.Report]
	stop     chan struct{}
	done     chan struct{}
}

func startSampler(interval time.Duration, log *slog.Logger) *sampler {
	s := &sampler{
		interval: interval,
		log:      log,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	s.latest.Store(&hostsensors.Report{HelperVersion: Version, Error: "starting"})
	go s.run()
	return s
}

func (s *sampler) run() {
	defer close(s.done)
	var sensor *intelPackageSensor
	defer func() { sensor.close() }()
	lastErr := ""
	next := time.NewTimer(0)
	defer next.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-next.C:
		}
		if sensor == nil {
			opened, err := openIntelPackageSensor()
			if err != nil {
				s.publishError(err.Error(), &lastErr)
				next.Reset(sensorRetryInterval)
				continue
			}
			sensor = opened
			s.log.Info("CPU package sensor open", "source", sourceIntelMSR, "tjmax_celsius", sensor.tjMax)
		}
		c, err := sensor.read()
		if err != nil {
			// A single invalid readout is transient; a dead executor is not.
			// Either way the last good number is not repeated.
			s.publishError(err.Error(), &lastErr)
			next.Reset(s.interval)
			continue
		}
		if lastErr != "" {
			s.log.Info("CPU package sensor readable again")
			lastErr = ""
		}
		s.latest.Store(&hostsensors.Report{
			HelperVersion: Version,
			CPU: &hostsensors.CPUReading{
				PackageCelsius: c,
				TjMaxCelsius:   sensor.tjMax,
				Source:         sourceIntelMSR,
				SampledAt:      time.Now().UTC(),
			},
		})
		next.Reset(s.interval)
	}
}

// publishError replaces the report with the reason there is no reading,
// logging it once per distinct reason rather than per tick.
func (s *sampler) publishError(msg string, last *string) {
	if *last != msg {
		s.log.Warn("CPU package sensor unavailable", "err", msg)
		*last = msg
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
