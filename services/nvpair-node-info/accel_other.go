// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package main

// detectAccelerators has no non-Linux implementation yet: the only supported
// accelerator source is the gasket/apex sysfs class (accel_linux.go). Other
// hosts report an empty list, exactly as before the field existed.
func detectAccelerators() []GPUInfo { return nil }
