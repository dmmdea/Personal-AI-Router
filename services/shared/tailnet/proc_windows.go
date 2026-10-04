// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package tailnet

import "syscall"

// hiddenProcAttr starts the CLI without a console window. PAIR's workers run
// detached from any console, so a console program they spawn would otherwise
// flash a new window on every call (HideWindow + CREATE_NO_WINDOW, matching every
// other NVPAIR subprocess).
func hiddenProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW
	}
}
