// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tailnet

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// fakeCLIEnv switches the test binary into a stand-in for the tailscale CLI, so
// runCLI is exercised against a real child process (exit codes, pipes, timeouts)
// without Tailscale installed.
const fakeCLIEnv = "NVPAIR_TAILNET_FAKE_CLI"

func TestMain(m *testing.M) {
	switch os.Getenv(fakeCLIEnv) {
	case "":
		os.Exit(m.Run())
	case "ok":
		fmt.Print(statusFixture)
		os.Exit(0)
	case "stopped-exit1":
		fmt.Print(`{"BackendState": "Stopped"}`)
		os.Exit(1)
	case "fail":
		fmt.Fprint(os.Stderr, "failed to connect to local tailscaled")
		os.Exit(1)
	case "hang":
		time.Sleep(30 * time.Second)
		os.Exit(0)
	case "flood":
		chunk := strings.Repeat("x", 1<<16)
		for i := 0; i < (maxStatusBytes/len(chunk))+4; i++ {
			fmt.Print(chunk)
		}
		os.Exit(0)
	}
	os.Exit(2)
}

func runFake(t *testing.T, mode string, timeout time.Duration) ([]byte, error) {
	t.Helper()
	t.Setenv(fakeCLIEnv, mode)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return runCLI(ctx, exe)
}

func TestRunCLIReturnsStatusDocument(t *testing.T) {
	out, err := runFake(t, "ok", 10*time.Second)
	if err != nil {
		t.Fatalf("runCLI: %v", err)
	}
	snap, err := Parse(out)
	if err != nil || !snap.Running || len(snap.Peers) != 5 {
		t.Fatalf("parsed %+v err=%v, want a running snapshot with the fixture's 5 peers", snap, err)
	}
}

func TestRunCLIKeepsAStoppedDocumentDespiteExitCode(t *testing.T) {
	out, err := runFake(t, "stopped-exit1", 10*time.Second)
	if err != nil {
		t.Fatalf("runCLI: %v", err)
	}
	if snap, err := Parse(out); err != nil || snap.Running {
		t.Fatalf("parsed %+v err=%v, want a not-running snapshot", snap, err)
	}
}

func TestRunCLIReportsFailureWithStderr(t *testing.T) {
	_, err := runFake(t, "fail", 10*time.Second)
	if err == nil || !strings.Contains(err.Error(), "failed to connect") {
		t.Fatalf("err = %v, want the CLI's stderr in the error", err)
	}
}

func TestFirstLineKeepsOnlyAShortFirstLine(t *testing.T) {
	in := "\n  failed to connect to local tailscaled  \nAuthURL: https://login.example/abc\n"
	if got := firstLine(in, 160); got != "failed to connect to local tailscaled" {
		t.Fatalf("firstLine = %q", got)
	}
	if got := firstLine(strings.Repeat("x", 500), 160); len(got) != 160 {
		t.Fatalf("firstLine kept %d bytes, want 160", len(got))
	}
}

func TestRunCLIHonoursTheDeadline(t *testing.T) {
	start := time.Now()
	_, err := runFake(t, "hang", 500*time.Millisecond)
	if err == nil {
		t.Fatal("runCLI returned no error for a hung CLI")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("runCLI took %v; the deadline did not stop the child", elapsed)
	}
}

func TestRunCLIRejectsOversizedOutput(t *testing.T) {
	_, err := runFake(t, "flood", 30*time.Second)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("err = %v, want an output-size error", err)
	}
}
