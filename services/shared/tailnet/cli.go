// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tailnet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// cliTimeout bounds one `status --json` call when the caller's context carries no
// deadline of its own. A wedged tailscaled must not stall discovery.
const cliTimeout = 5 * time.Second

// maxStatusBytes caps how much CLI output is read. A large tailnet's status is
// a few hundred kilobytes; anything past this is not a status document.
const maxStatusBytes = 8 << 20

// CLIRunner returns a Runner that executes the local Tailscale CLI, or nil when
// no client is installed (Status then reports ErrUnavailable).
//
// The binary is looked up at its platform install locations first and on PATH
// only as a last resort. The install locations are writable by administrators
// only, so a directory an unprivileged user can prepend to PATH cannot put its
// own "tailscale" in front of the real one.
func CLIRunner() Runner {
	bin := findCLI()
	if bin == "" {
		return nil
	}
	return func(ctx context.Context) ([]byte, error) {
		return runCLI(ctx, bin)
	}
}

func findCLI() string {
	for _, p := range installCandidates() {
		if p == "" {
			continue
		}
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	name := "tailscale"
	if runtime.GOOS == "windows" {
		name = "tailscale.exe"
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return ""
}

// installCandidates lists where each platform's installer puts the CLI.
func installCandidates() []string {
	switch runtime.GOOS {
	case "windows":
		var out []string
		for _, env := range []string{"ProgramFiles", "ProgramW6432"} {
			if dir := os.Getenv(env); dir != "" {
				out = append(out, filepath.Join(dir, "Tailscale", "tailscale.exe"))
			}
		}
		return out
	case "darwin":
		return []string{
			"/Applications/Tailscale.app/Contents/MacOS/Tailscale",
			"/usr/local/bin/tailscale",
			"/opt/homebrew/bin/tailscale",
		}
	default:
		return []string{"/usr/bin/tailscale", "/usr/sbin/tailscale", "/usr/local/bin/tailscale"}
	}
}

func runCLI(ctx context.Context, bin string) ([]byte, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cliTimeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, bin, "status", "--json")
	cmd.SysProcAttr = hiddenProcAttr()
	// A child that is killed but left a grandchild holding its pipes must not
	// keep Wait (and so the caller) blocked.
	cmd.WaitDelay = 2 * time.Second
	stdout := &cappedBuffer{limit: maxStatusBytes}
	var stderr cappedBuffer
	stderr.limit = 4 << 10
	cmd.Stdout = stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if stdout.overflow {
		return nil, fmt.Errorf("tailscale status output exceeds %d bytes", maxStatusBytes)
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("tailscale status: %w", ctx.Err())
		}
		// A stopped or logged-out client may still print a valid document with a
		// non-Running BackendState before exiting non-zero; that is a state worth
		// reporting, not a failure.
		if out := bytes.TrimSpace(stdout.Bytes()); len(out) > 0 && out[0] == '{' {
			return out, nil
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil, fmt.Errorf("tailscale status exited %d: %s", exitErr.ExitCode(), firstLine(stderr.String(), 160))
		}
		return nil, fmt.Errorf("tailscale status: %w", err)
	}
	return stdout.Bytes(), nil
}

// firstLine returns the first non-empty line of s, cut to max bytes. The CLI's
// stderr explains a failure in its first line ("failed to connect to local
// tailscaled ..."); keeping only that keeps anything longer out of the logs.
func firstLine(s string, max int) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			if len(line) > max {
				line = line[:max]
			}
			return line
		}
	}
	return ""
}

// cappedBuffer stores at most limit bytes and records whether more arrived.
//
// The buffer is a named field, not embedded: an embedded bytes.Buffer would
// promote ReadFrom, and io.Copy (which exec.Cmd uses to drain the pipe) prefers
// ReadFrom over Write — the cap would never run.
type cappedBuffer struct {
	buf      bytes.Buffer
	limit    int
	overflow bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	room := b.limit - b.buf.Len()
	if room <= 0 {
		b.overflow = b.overflow || len(p) > 0
		return len(p), nil
	}
	if len(p) > room {
		b.overflow = true
		b.buf.Write(p[:room])
		return len(p), nil
	}
	return b.buf.Write(p)
}

func (b *cappedBuffer) Bytes() []byte  { return b.buf.Bytes() }
func (b *cappedBuffer) String() string { return b.buf.String() }
