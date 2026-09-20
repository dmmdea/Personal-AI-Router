// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"fmt"
	"os"
)

// nvpair-sensors exists for Windows, where the CPU package sensor needs a
// privileged reader. Linux nvpair-node-info reads hwmon directly and macOS
// has no equivalent, so this build only answers --version.
func main() {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println(Version)
		return
	}
	fmt.Fprintln(os.Stderr, "nvpair-sensors is the Windows host-sensor helper; it has nothing to do on this platform")
	os.Exit(2)
}
