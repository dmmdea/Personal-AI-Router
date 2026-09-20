// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !linux && !windows

package main

// detectAccelerators has no implementation on this platform: the supported
// accelerator sources are the gasket/apex sysfs class (accel_linux.go) and
// HailoRT on Windows (accel_windows.go). Other hosts report an empty list,
// exactly as before the field existed.
func detectAccelerators() []GPUInfo { return nil }
