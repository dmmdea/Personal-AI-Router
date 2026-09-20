// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useCallback, useMemo, useState } from 'react'
import { Flex, Stack, Text } from '@nvidia/foundations-react-core'
import SvgLineChart, { type MetricDataset } from '@/ui/components/SvgLineChart'
import type { NodeItem } from '@/shared/types/nodes'
import { useMetricsStore } from '@/ui/stores/metrics.store'
import { getGpuColor, getVramColor } from '@/ui/utils/colors'
import { formatBytes } from '@/ui/utils/formatters'
import { CHART_COLORS } from '@/ui/constants/colors'

export default function NodePerformance({
    node,
    className
}: {
    node: NodeItem
    className?: string
}) {
    const nodeMetrics = useMetricsStore(state => state.nodeMetrics.get(node.id))
    const [selectedMetric, setSelectedMetric] = useState<string | undefined>(undefined)

    const handleLegendClick = useCallback((metricKey: string) => {
        // If clicking the same metric that's already selected, show all
        // Otherwise, show only the clicked metric (solo it)
        setSelectedMetric(prev => (prev === metricKey ? undefined : metricKey))
    }, [])

    // The detailed performance view intentionally charts EVERY detected GPU,
    // including any the radial omits (the radial filters to inference-ready GPUs
    // via inferenceHardwareIds, src/ui/utils/gpu-inference.ts). Here the user is
    // inspecting raw hardware activity, so an iGPU's utilization is useful signal
    // rather than noise.
    const datasets = useMemo(() => {
        if (!nodeMetrics) return []

        const gpuCount = nodeMetrics.gpuUtilization.length
        const result: MetricDataset[] = []

        // Add GPU utilization datasets (cycling through color palette)
        nodeMetrics.gpuUtilization.forEach((gpu, index) => {
            const color = getGpuColor(index)
            result.push({
                data: gpu.data,
                label: gpuCount > 1 ? `GPU ${index}` : 'GPU',
                color,
                key: `gpu-${gpu.id}`
            })
        })

        // Add CPU
        result.push({
            data: nodeMetrics.cpuUtilization,
            label: 'CPU',
            color: CHART_COLORS.CPU,
            key: 'cpu'
        })

        // Add RAM
        result.push({
            data: nodeMetrics.memoryUsage,
            label: 'RAM',
            color: CHART_COLORS.MEMORY,
            key: 'memory'
        })

        // Add VRAM datasets (cycling through color palette)
        nodeMetrics.gpuVramUsage.forEach((gpu, index) => {
            const color = getVramColor(index)
            result.push({
                data: gpu.data,
                label: gpuCount > 1 ? `VRAM ${index}` : 'VRAM',
                color,
                key: `vram-${gpu.id}`
            })
        })

        return result
    }, [nodeMetrics])

    const hardwareInfo = useMemo(() => {
        const totalStorage = node.topology.storage.reduce(
            (sum, storage) => sum + storage.capacity,
            0
        )

        // Helper to get the latest value from a metric data array
        const getLatestValue = (data: { timestamp: number; value: number }[]) => {
            if (data.length === 0) return 0
            const value = data[data.length - 1].value
            return value ?? 0 // Treat undefined as 0
        }

        return {
            // Detailed view lists every detected GPU (incl. iGPUs); only the
            // radial filters to inference-ready GPUs (via inferenceHardwareIds).
            gpus: node.topology.gpus.map((gpu, index) => {
                const gpuUtilData = nodeMetrics?.gpuUtilization[index]
                const gpuVramData = nodeMetrics?.gpuVramUsage[index]

                return {
                    id: gpu.id,
                    name: gpu.name,
                    vramTotal: gpu.vramTotal,
                    vramFormatted: formatBytes(gpu.vramTotal, 1),
                    utilization: gpuUtilData ? getLatestValue(gpuUtilData.data) : 0,
                    vramUsage: gpuVramData ? getLatestValue(gpuVramData.data) : 0,
                    color: getGpuColor(index),
                    vramColor: getVramColor(index),
                    temperature: nodeMetrics?.gpuTemperature.find(t => t.id === gpu.id)?.value ?? 0,
                    kind: gpu.kind
                }
            }),
            cpuCores: node.topology.cpu.cores,
            cpuModel: node.topology.cpu.model,
            cpuUtilization: nodeMetrics ? getLatestValue(nodeMetrics.cpuUtilization) : 0,
            cpuTemperature: nodeMetrics?.cpuTemperature ?? 0,
            memory: formatBytes(node.topology.ram, 1),
            memoryTotal: node.topology.ram,
            memoryUsage: nodeMetrics ? getLatestValue(nodeMetrics.memoryUsage) : 0,
            storage: formatBytes(totalStorage, 1)
        }
    }, [node, nodeMetrics])

    return (
        // The panel is its own query container so the chart/legend split follows
        // the CARD's width, not the window's: the node list renders one, two or
        // three cards per row, so a viewport media query sized the legend wrong
        // at every window width. Containment is scoped to this wrapper rather
        // than to .node-card so the card's tooltips and menus keep their
        // original containing block.
        <div className={`@container w-full min-w-0 ${className ? className : ''}`}>
            <Flex
                // overflow-x-auto is the last resort only: with the stacked
                // layout below 720px and the minimums below, the body scrolls
                // instead of clipping a legend that still cannot fit.
                className="w-full min-w-0 flex-nowrap overflow-x-auto @max-[720px]:flex-col"
                align="stretch"
                gap="2"
            >
                {/* Chart Section (placeholder when no metrics) */}
                <div className="grow min-w-[320px] @max-[720px]:min-w-0 overflow-hidden min-h-[200px] relative">
                    <SvgLineChart
                        datasets={datasets}
                        maxDataPoints={30}
                        yAxisMax={100}
                        selectedKey={selectedMetric}
                    />
                    {!nodeMetrics && (
                        <div className="absolute inset-0 flex items-center justify-center pointer-events-none">
                            <Text kind="body/regular/sm" className="text-subtle-color">
                                No metrics yet
                            </Text>
                        </div>
                    )}
                </div>

                {/* Custom Legend / Hardware Info Section */}
                <Stack
                    gap="4"
                    // Grows with the card between a 260px floor (fits the
                    // longest device names on two lines) and a 420px ceiling so
                    // the chart keeps the remainder. Stacked under the chart the
                    // basis would apply to the block axis, so it is reset there.
                    className="flex-[0_1_clamp(260px,32%,420px)] min-w-[260px] ml-2 self-center @max-[720px]:flex-none @max-[720px]:ml-0 @max-[720px]:w-full @max-[720px]:self-stretch"
                >
                    {/* GPU Metrics */}
                    {hardwareInfo.gpus.map(gpu => {
                        // Check if this GPU's metrics are selected
                        const isGpuSelected =
                            selectedMetric === undefined ||
                            selectedMetric === `gpu-${gpu.id}` ||
                            selectedMetric === `vram-${gpu.id}`

                        return (
                            <Stack gap="0" key={gpu.id}>
                                <Text
                                    kind="body/semibold/sm"
                                    // Wraps onto a second line rather than being cut
                                    // with an ellipsis; the title keeps the full name
                                    // available on hover either way.
                                    className="transition-opacity break-words"
                                    style={{ opacity: isGpuSelected ? 1 : 0.5 }}
                                    title={gpu.name}
                                >
                                    {gpu.name}
                                </Text>

                                <Stack gap="1">
                                    {/* GPU Utilization */}
                                    <Flex
                                        align="center"
                                        gap="2"
                                        className="cursor-pointer"
                                        onClick={() => handleLegendClick(`gpu-${gpu.id}`)}
                                    >
                                        <div
                                            className="w-3 h-3 min-w-3 min-h-3 max-w-3 max-h-3 rounded-full transition-opacity"
                                            style={{
                                                backgroundColor: `${gpu.color}BF`,
                                                border: `2px solid ${gpu.color}`,
                                                marginTop: '1px',
                                                opacity:
                                                    selectedMetric === undefined ||
                                                    selectedMetric === `gpu-${gpu.id}`
                                                        ? 1
                                                        : 0.3
                                            }}
                                        />

                                        <Flex
                                            align="center"
                                            gap="2"
                                            className="transition-opacity"
                                            style={{
                                                opacity:
                                                    selectedMetric === undefined ||
                                                    selectedMetric === `gpu-${gpu.id}`
                                                        ? 1
                                                        : 0.5
                                            }}
                                        >
                                            <Text kind="body/regular/sm">Usage</Text>
                                            <Text kind="body/regular/sm">
                                                {Math.floor(gpu.utilization)}%
                                            </Text>
                                        </Flex>
                                    </Flex>

                                    {/* GPU VRAM — skipped for an accelerator row (no VRAM figure) */}
                                    {!(gpu.kind === 'npu' && gpu.vramTotal === 0) && (
                                        <Flex
                                            align="center"
                                            gap="2"
                                            className="cursor-pointer"
                                            onClick={() => handleLegendClick(`vram-${gpu.id}`)}
                                        >
                                            <div
                                                className="w-3 h-3 min-w-3 min-h-3 max-w-3 max-h-3 rounded-full transition-opacity"
                                                style={{
                                                    backgroundColor: `${gpu.vramColor}BF`,
                                                    border: `2px solid ${gpu.vramColor}`,
                                                    marginTop: '1px',
                                                    opacity:
                                                        selectedMetric === undefined ||
                                                        selectedMetric === `vram-${gpu.id}`
                                                            ? 1
                                                            : 0.3
                                                }}
                                            />
                                            <Flex
                                                align="center"
                                                gap="2"
                                                className="transition-opacity"
                                                style={{
                                                    opacity:
                                                        selectedMetric === undefined ||
                                                        selectedMetric === `vram-${gpu.id}`
                                                            ? 1
                                                            : 0.5
                                                }}
                                            >
                                                <Text kind="body/regular/sm">VRAM</Text>
                                                <Text kind="body/regular/sm">
                                                    {formatBytes(
                                                        Math.floor(
                                                            (gpu.vramTotal * gpu.vramUsage) / 100
                                                        ),
                                                        1
                                                    )}{' '}
                                                    / {gpu.vramFormatted}
                                                </Text>
                                            </Flex>
                                        </Flex>
                                    )}
                                    {/* GPU temperature — a reading, not a chart series. A
                                    device without one (an integrated GPU, a host without
                                    nvidia-smi) gets no row rather than a placeholder. */}
                                    {gpu.temperature > 0 && (
                                        <Flex align="center" gap="2">
                                            <div
                                                className="w-3 h-3 min-w-3 min-h-3 max-w-3 max-h-3 rounded-full transition-opacity"
                                                style={{
                                                    backgroundColor: `${gpu.color}BF`,
                                                    border: `2px solid ${gpu.color}`,
                                                    marginTop: '1px',
                                                    opacity: isGpuSelected ? 1 : 0.3
                                                }}
                                            />
                                            <Flex
                                                align="center"
                                                gap="2"
                                                className="transition-opacity"
                                                style={{ opacity: isGpuSelected ? 1 : 0.5 }}
                                            >
                                                <Text kind="body/regular/sm">Temp</Text>
                                                <Text kind="body/regular/sm">
                                                    {gpu.temperature} °C
                                                </Text>
                                            </Flex>
                                        </Flex>
                                    )}
                                </Stack>
                            </Stack>
                        )
                    })}

                    {/* CPU */}
                    <Flex
                        align="start"
                        gap="2"
                        className="cursor-pointer"
                        onClick={() => handleLegendClick('cpu')}
                    >
                        <div
                            className="w-3 h-3 min-w-3 min-h-3 max-w-3 max-h-3 rounded-full transition-opacity"
                            style={{
                                backgroundColor: `${CHART_COLORS.CPU}BF`,
                                border: `2px solid ${CHART_COLORS.CPU}`,
                                marginTop: '4px',
                                opacity:
                                    selectedMetric === undefined || selectedMetric === 'cpu'
                                        ? 1
                                        : 0.3
                            }}
                        />
                        <Stack
                            className="min-w-0 transition-opacity"
                            style={{
                                opacity:
                                    selectedMetric === undefined || selectedMetric === 'cpu'
                                        ? 1
                                        : 0.5
                            }}
                        >
                            <Text
                                kind="body/semibold/sm"
                                className="break-words"
                                title={hardwareInfo.cpuModel}
                            >
                                {hardwareInfo.cpuModel}
                            </Text>
                            <Text kind="body/regular/sm">
                                {Math.floor(hardwareInfo.cpuUtilization)}%{' '}
                                {hardwareInfo.cpuCores ? `(${hardwareInfo.cpuCores} cores)` : ''}
                                {hardwareInfo.cpuTemperature > 0
                                    ? ` · ${hardwareInfo.cpuTemperature} °C`
                                    : ''}
                            </Text>
                        </Stack>
                    </Flex>

                    {/* Memory */}
                    <Flex
                        align="start"
                        gap="2"
                        className="cursor-pointer"
                        onClick={() => handleLegendClick('memory')}
                    >
                        <div
                            className="w-3 h-3 min-w-3 min-h-3 max-w-3 max-h-3 rounded-full transition-opacity"
                            style={{
                                backgroundColor: `${CHART_COLORS.MEMORY}BF`,
                                border: `2px solid ${CHART_COLORS.MEMORY}`,
                                marginTop: '4px',
                                opacity:
                                    selectedMetric === undefined || selectedMetric === 'memory'
                                        ? 1
                                        : 0.3
                            }}
                        />
                        <Stack
                            className="transition-opacity"
                            style={{
                                opacity:
                                    selectedMetric === undefined || selectedMetric === 'memory'
                                        ? 1
                                        : 0.5
                            }}
                        >
                            <Text kind="body/semibold/sm">Memory</Text>
                            <Text kind="body/regular/sm">
                                {formatBytes(
                                    Math.floor(
                                        (hardwareInfo.memoryTotal * hardwareInfo.memoryUsage) / 100
                                    ),
                                    1
                                )}{' '}
                                / {hardwareInfo.memory}
                            </Text>
                        </Stack>
                    </Flex>
                </Stack>
            </Flex>
        </div>
    )
}
