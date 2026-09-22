// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"

	"nvpair-shared/hostsensors"
)

// nvpair-sensors is the elevated host-sensor helper for Windows. It samples
// the CPU package temperature through PawnIO and answers every connection to
// its named pipe with one JSON report (nvpair-shared/hostsensors).
//
// Modes, by flag:
//
//	(none)       run in the foreground — as the service when started by the
//	             service control manager, else until Ctrl-C (needs elevation)
//	--install    register and start the Windows service (elevation)
//	--uninstall  stop and remove the service (elevation)
//	--probe      read one report from the running service and print it; any
//	             user, exit 0 with a CPU reading, 2 without one, 1 unreachable
//	--once       read the sensor directly and print one report (elevation)

// onceProbeInterval is how long --once waits between its two energy-counter
// reads. It matches the service's default sampling interval, so the wattage a
// one-shot prints is averaged over the same window the service publishes.
const onceProbeInterval = 2 * time.Second

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	install := flag.Bool("install", false, "register this executable as the Windows service and start it (administrator)")
	uninstall := flag.Bool("uninstall", false, "stop and remove the Windows service (administrator)")
	probe := flag.Bool("probe", false, "read one report from the running service over its pipe and print it (any user)")
	once := flag.Bool("once", false, "read the sensor directly, print one report and exit (administrator)")
	pipe := flag.String("pipe", hostsensors.PipeName, "named pipe to serve reports on")
	interval := flag.Duration("interval", 2*time.Second, "how often the sensor is sampled")
	flag.Parse()

	switch {
	case *showVersion:
		fmt.Println(Version)
		return
	case *install:
		exe, err := os.Executable()
		if err != nil {
			fail("resolve executable path: %v", err)
		}
		if err := installService(exe); err != nil {
			fail("%v", err)
		}
		if err := startService(); err != nil {
			fail("installed, but: %v", err)
		}
		fmt.Printf("installed and started service %s (%s)\n", serviceName, exe)
		return
	case *uninstall:
		if err := uninstallService(); err != nil {
			fail("%v", err)
		}
		fmt.Printf("removed service %s\n", serviceName)
		return
	case *probe:
		r, err := hostsensors.Read(2 * time.Second)
		if err != nil {
			fail("helper not reachable at %s: %v", hostsensors.PipeName, err)
		}
		if err := hostsensors.Encode(os.Stdout, r); err != nil {
			fail("%v", err)
		}
		if r.CPU == nil {
			os.Exit(2)
		}
		return
	case *once:
		s, err := openIntelPackageSensor()
		if err != nil {
			fail("%v", err)
		}
		defer s.close()
		c, err := s.read()
		if err != nil {
			fail("%v", err)
		}
		// Package power is a derivative of an energy counter, so one read
		// yields nothing. Prime the baseline, wait one interval, and read
		// again — otherwise --once would print a report whose missing
		// wattage says "this CPU has no energy counter" when it only means
		// "you asked once".
		s.power(time.Now())
		time.Sleep(onceProbeInterval)
		watts, _ := s.power(time.Now())
		r := hostsensors.Report{HelperVersion: Version, CPU: &hostsensors.CPUReading{
			PackageCelsius: c, PackageWatts: watts, TjMaxCelsius: s.tjMax(),
			Source: sourceIntelMSR, SampledAt: time.Now().UTC(),
		}}
		if err := hostsensors.Encode(os.Stdout, r); err != nil {
			fail("%v", err)
		}
		return
	}

	isService, err := svc.IsWindowsService()
	if err != nil {
		fail("determine service context: %v", err)
	}
	if isService {
		elog, err := eventlog.Open(serviceName)
		if err != nil {
			os.Exit(1)
		}
		defer elog.Close()
		log := slog.New(&eventLogHandler{l: elog})
		log.Info("starting", "version", Version, "pipe", *pipe, "interval", interval.String())
		h := &serviceHandler{
			run: func(ctx context.Context) error { return runHelper(ctx, *pipe, *interval, log) },
			log: log,
		}
		if err := svc.Run(serviceName, h); err != nil {
			elog.Error(1, fmt.Sprintf("service run failed: %v", err))
			os.Exit(1)
		}
		return
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Info("starting in the foreground", "version", Version, "pipe", *pipe, "interval", interval.String())
	if err := runHelper(ctx, *pipe, *interval, log); err != nil {
		log.Error("helper stopped", "err", err)
		os.Exit(1)
	}
}

// runHelper samples until ctx ends and serves the latest report on the pipe.
func runHelper(ctx context.Context, pipe string, interval time.Duration, log *slog.Logger) error {
	s := startSampler(interval, log, openPackageSensor)
	defer s.Stop()
	return servePipe(ctx, pipe, s.report, log)
}

// warn prints a note to stderr without ending the process, for the
// best-effort sections of a one-shot read.
func warn(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "nvpair-sensors: "+format+"\n", args...)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "nvpair-sensors: "+format+"\n", args...)
	os.Exit(1)
}
