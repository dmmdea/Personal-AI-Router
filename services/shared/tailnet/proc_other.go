// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package tailnet

import "syscall"

// hiddenProcAttr is a no-op outside Windows: there is no console window to hide.
func hiddenProcAttr() *syscall.SysProcAttr { return nil }
