// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { memo } from 'react'
import { Flex, Stack, Text } from '@nvidia/foundations-react-core'
import type { CpuFallbackInfo, GpuInfo } from '@/ui/types/node-hardware'
import {
    memoryLabel,
    memoryLineValue,
    showsMemoryLine,
    thermalLine
} from '@/ui/utils/hardware-rows'

const dotSize = '10px'

function ColorDot({ color }: { color: string }) {
    return (
        <div
            style={{
                width: dotSize,
                height: dotSize,
                borderRadius: '50%',
                backgroundColor: `${color}AA`,
                border: `2px solid ${color}`
            }}
        />
    )
}

function NodeChartLegend({
    gpuInfo,
    cpuFallbackInfo
}: {
    gpuInfo: GpuInfo[]
    cpuFallbackInfo?: CpuFallbackInfo | null
}) {
    if (cpuFallbackInfo) {
        const cpuThermal = thermalLine(cpuFallbackInfo.temperature, cpuFallbackInfo.power)
        return (
            <Stack gap="1">
                <Text kind="body/semibold/sm">{cpuFallbackInfo.model}</Text>
                <Flex align="center" gap="2">
                    <ColorDot color={cpuFallbackInfo.usageColor} />
                    <Flex align="center" gap="2">
                        <Text kind="body/regular/sm" className="text-subtle-color">
                            CPU
                        </Text>
                        <Text kind="body/semibold/sm">{cpuFallbackInfo.usage}%</Text>
                        {/* "55 °C (140 W)", the wattage only where something
                            meters the package, and "140 W" alone on a host
                            that meters but cannot read a temperature. */}
                        {cpuThermal && (
                            <Text kind="body/regular/sm" className="text-subtle-color">
                                {cpuThermal.value}
                            </Text>
                        )}
                    </Flex>
                </Flex>
                <Flex align="center" gap="2">
                    <ColorDot color={cpuFallbackInfo.memoryColor} />
                    <Text kind="body/regular/sm" className="text-subtle-color">
                        RAM
                    </Text>
                    <Text kind="body/semibold/sm">
                        {cpuFallbackInfo.memoryUsageFormatted} /{' '}
                        {cpuFallbackInfo.memoryTotalFormatted}
                    </Text>
                </Flex>
            </Stack>
        )
    }

    return (
        <Stack gap="3">
            {gpuInfo.map(gpu => {
                const thermal = thermalLine(gpu.temperature, gpu.power)
                return (
                    <Stack key={gpu.id} gap="1">
                        <Text kind="body/semibold/sm">{gpu.name}</Text>
                        {
                            <Flex align="center" gap="2">
                                <ColorDot color={gpu.usageColor} />
                                <Flex align="center" gap="2">
                                    <Text kind="body/regular/sm" className="text-subtle-color">
                                        Usage
                                    </Text>
                                    <Text kind="body/semibold/sm">{gpu.usage}%</Text>
                                </Flex>
                            </Flex>
                        }
                        {/* An accelerator (Edge TPU / NPU) may have no VRAM figure; the row would only ever
                        read "0 B / 0 B". A device sharing the host's memory
                        reads "Shared" — and gets a line only when something
                        measured the pool, because a bare shared ceiling is a
                        static number among live ones. See hardware-rows.ts. */}
                        {showsMemoryLine(gpu, gpu.vramUsedBytes) && (
                            <Flex align="center" gap="2">
                                <ColorDot color={gpu.vramColor} />
                                <Text kind="body/regular/sm" className="text-subtle-color">
                                    {memoryLabel(gpu)}
                                </Text>
                                <Text kind="body/semibold/sm">
                                    {memoryLineValue(gpu, gpu.vramUsedBytes)}
                                </Text>
                            </Flex>
                        )}
                        {/* "Temp 50 °C (200 W)" — one line for one device state.
                        A device with neither reading (an integrated GPU, a
                        host without nvidia-smi) gets no row rather than a
                        placeholder; one that is metered but has no thermal
                        readout gets "Power 200 W" instead. */}
                        {thermal && (
                            <Flex align="center" gap="2">
                                <ColorDot color={gpu.usageColor} />
                                <Text kind="body/regular/sm" className="text-subtle-color">
                                    {thermal.label}
                                </Text>
                                <Text kind="body/semibold/sm">{thermal.value}</Text>
                            </Flex>
                        )}
                    </Stack>
                )
            })}
        </Stack>
    )
}

export default memo(NodeChartLegend)
