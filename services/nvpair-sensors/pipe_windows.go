// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/Microsoft/go-winio"

	"nvpair-shared/hostsensors"
)

// pipeSDDL: SYSTEM and Administrators have full control; any authenticated
// local user may connect (a client opens the pipe read/write even though it
// only reads, so both generic rights are granted). go-winio creates the pipe
// with PIPE_REJECT_REMOTE_CLIENTS, so the report never leaves the host.
const pipeSDDL = "D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GRGW;;;AU)"

// pipeWriteTimeout bounds one client's read of the report so a stalled
// client cannot pin a server goroutine.
const pipeWriteTimeout = 2 * time.Second

// servePipe answers every connection with one report and closes it. It
// returns when ctx is cancelled, or with the error that stopped the listener.
func servePipe(ctx context.Context, name string, report func() hostsensors.Report, log *slog.Logger) error {
	l, err := winio.ListenPipe(name, &winio.PipeConfig{SecurityDescriptor: pipeSDDL})
	if err != nil {
		return fmt.Errorf("listen on %s: %w", name, err)
	}
	go func() {
		<-ctx.Done()
		l.Close()
	}()
	log.Info("serving host sensors", "pipe", name)
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept on %s: %w", name, err)
		}
		go answer(c, report(), log)
	}
}

func answer(c net.Conn, r hostsensors.Report, log *slog.Logger) {
	defer c.Close()
	if err := c.SetWriteDeadline(time.Now().Add(pipeWriteTimeout)); err != nil {
		log.Debug("set write deadline", "err", err)
		return
	}
	if err := hostsensors.Encode(c, r); err != nil {
		log.Debug("write report", "err", err)
	}
}
