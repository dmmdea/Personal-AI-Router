// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import "sync"

// systemMemTotal is the host's memory total, read once and shared by every
// consumer: memory.total_bytes in the response, and the VramBytes ceiling of
// every unified-memory row (an Intel or AMD integrated GPU on Windows, an
// Intel iGPU, a Mali GPU, an RKNPU or an nvidia UMA part on Linux). The number
// does not change at runtime, and one read keeps those figures identical.
//
// The total is on the SAME BASE as the collector's used figure, so used/total
// is a true fraction and a reserved byte is never in one number but not the
// other. That base is the memory the operating system manages, not the
// installed DIMM capacity:
//
//   - Linux: /proc/meminfo MemTotal, the file used = MemTotal - MemAvailable
//     comes from. It excludes firmware-reserved ranges and the crash kernel.
//   - Windows: GlobalMemoryStatusEx TotalPhys, the call used = TotalPhys -
//     AvailPhys comes from. It excludes hardware-reserved memory (an iGPU's
//     stolen aperture, for one).
//   - macOS: gopsutil's hw.memsize, the same reader that produces used.
//
// Installed capacity was the old Windows answer (the sum of the DIMMs) and the
// old Linux one was worse: ghw counts every online sysfs memory block in full,
// and the blocks that straddle the PCI hole or end past the last byte of RAM
// are only partly RAM. A 64 GiB desktop with 2 GiB blocks published 66 GiB; a
// 32 GB laptop with 128 MiB blocks published 1.25 GiB more than the kernel
// manages, and read 2.2 points low on RAM % as a result. Installed capacity is
// not readable unprivileged on Linux in any case (the SMBIOS memory-device
// tables are root-only), so it could not be the shared base.
var systemMemTotal = sync.OnceValue(detectMemoryTotal)
