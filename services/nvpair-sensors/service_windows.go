// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	serviceName        = "nvpair-sensors"
	serviceDisplayName = "NVIDIA PAIR host sensors"
	serviceDescription = "Reads the CPU package temperature through the PawnIO driver and serves it to nvpair-node-info over a local named pipe."

	serviceStopTimeout = 15 * time.Second
)

// serviceHandler adapts the helper to the service control manager: run the
// helper until Stop or Shutdown, report an exit code if it dies on its own.
type serviceHandler struct {
	run func(ctx context.Context) error
	log *slog.Logger
}

func (h *serviceHandler) Execute(_ []string, requests <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.run(ctx) }()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case req := <-requests:
			switch req.Cmd {
			case svc.Interrogate:
				status <- req.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				cancel()
				<-done
				return false, 0
			}
		case err := <-done:
			status <- svc.Status{State: svc.StopPending}
			if err != nil {
				h.log.Error("helper stopped", "err", err)
				return true, 1
			}
			return false, 0
		}
	}
}

// installService registers exePath as an automatic-start LocalSystem
// service with restart-on-failure, and registers the event-log source.
func installService(exePath string) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to the service manager: %w", err)
	}
	defer m.Disconnect()
	if s, err := m.OpenService(serviceName); err == nil {
		s.Close()
		return fmt.Errorf("service %s already exists; run --uninstall first", serviceName)
	}
	s, err := m.CreateService(serviceName, exePath, mgr.Config{
		DisplayName:  serviceDisplayName,
		Description:  serviceDescription,
		StartType:    mgr.StartAutomatic,
		ErrorControl: mgr.ErrorNormal,
	})
	if err != nil {
		return fmt.Errorf("create service: %w", err)
	}
	defer s.Close()
	if err := s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 5 * time.Minute},
	}, 24*60*60); err != nil {
		s.Delete()
		return fmt.Errorf("set recovery actions: %w", err)
	}
	if err := eventlog.InstallAsEventCreate(serviceName, eventlog.Error|eventlog.Warning|eventlog.Info); err != nil && !strings.Contains(err.Error(), "already exists") {
		s.Delete()
		return fmt.Errorf("register event source: %w", err)
	}
	return nil
}

func startService() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to the service manager: %w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("open service %s: %w", serviceName, err)
	}
	defer s.Close()
	if err := s.Start(); err != nil {
		return fmt.Errorf("start service %s: %w", serviceName, err)
	}
	return nil
}

// uninstallService stops the service if it runs, deletes it and removes the
// event-log source.
func uninstallService() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to the service manager: %w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("service %s is not installed: %w", serviceName, err)
	}
	defer s.Close()
	if st, err := s.Query(); err == nil && st.State != svc.Stopped {
		if _, err := s.Control(svc.Stop); err != nil {
			return fmt.Errorf("stop service %s: %w", serviceName, err)
		}
		deadline := time.Now().Add(serviceStopTimeout)
		for {
			st, err := s.Query()
			if err != nil || st.State == svc.Stopped {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("service %s did not stop within %s", serviceName, serviceStopTimeout)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	if err := s.Delete(); err != nil {
		return fmt.Errorf("delete service %s: %w", serviceName, err)
	}
	if err := eventlog.Remove(serviceName); err != nil && !strings.Contains(err.Error(), "cannot find") {
		return fmt.Errorf("remove event source: %w", err)
	}
	return nil
}

// eventLogHandler is a minimal slog.Handler onto the Windows event log for
// service mode, where stderr goes nowhere. Levels map to the three event
// types; attributes are appended as key=value.
type eventLogHandler struct {
	l     *eventlog.Log
	attrs []slog.Attr
}

func (h *eventLogHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelInfo
}

func (h *eventLogHandler) Handle(_ context.Context, r slog.Record) error {
	var sb strings.Builder
	sb.WriteString(r.Message)
	write := func(a slog.Attr) bool {
		sb.WriteString(" ")
		sb.WriteString(a.Key)
		sb.WriteString("=")
		sb.WriteString(a.Value.String())
		return true
	}
	for _, a := range h.attrs {
		write(a)
	}
	r.Attrs(write)
	msg := sb.String()
	switch {
	case r.Level >= slog.LevelError:
		return h.l.Error(1, msg)
	case r.Level >= slog.LevelWarn:
		return h.l.Warning(1, msg)
	default:
		return h.l.Info(1, msg)
	}
}

func (h *eventLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	merged := make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	merged = append(merged, h.attrs...)
	merged = append(merged, attrs...)
	return &eventLogHandler{l: h.l, attrs: merged}
}

func (h *eventLogHandler) WithGroup(string) slog.Handler { return h }
