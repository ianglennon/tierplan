// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { StatsPoint } from "@/features/stats/use-stats-history";

export interface ChartSeries {
  key: "cpu" | "mem" | "netRx" | "netTx" | "blkRead" | "blkWrite";
  label: string;
  dashed?: boolean;
}

interface StatsChartProps {
  title: string;
  points: StatsPoint[];
  series: ChartSeries[];
  format: (value: number | null) => string;
  floor?: number;
  limit?: number | null;
  limitLabel?: string;
}

export function StatsChart(_props: StatsChartProps) {
  return null;
}
