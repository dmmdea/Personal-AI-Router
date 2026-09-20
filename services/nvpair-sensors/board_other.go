// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import "errors"

// openBoardSensor has no implementation off Windows. The Super I/O chip is
// reached through the signed PawnIO driver, which is a Windows kernel
// component; a Linux host reads the same chip through the kernel's own
// nct6775 hwmon driver and needs no helper at all.
func openBoardSensor() (boardSensor, error) {
	return nil, errors.New("motherboard sensors are read through the Windows PawnIO driver only")
}
