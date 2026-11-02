// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Area, AreaChart, CartesianGrid, ReferenceLine, XAxis, YAxis } from "recharts";

import {
  type ChartConfig,
  ChartContainer,
  ChartTooltip,
  ChartTooltipContent,
} from "@/components/ui/chart";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { isGap, type StatsPoint } from "@/features/stats/use-stats-history";
import { useT } from "@/i18n/use-t";

export interface ChartSeries {
  key: "cpu" | "mem" | "netRx" | "netTx" | "blkRead" | "blkWrite";
  label: string;
  // A secondary series is drawn dashed so it differs from the first by line style and by
  // its legend text, never by colour (UI-SPEC "Charts and sparklines").
  dashed?: boolean;
}

interface StatsChartProps {
  title: string;
  points: StatsPoint[];
  series: ChartSeries[];
  // How a value is written: on the axis, in the tooltip and in the summary.
  format: (value: number | null) => string;
  // The smallest the y-axis top may be: CPU's is 100, so one full core is always in view.
  floor?: number;
  // A reference line: the memory limit, in the chart's own unit.
  limit?: number | null;
  limitLabel?: string;
}

// The longest legend label that is not cut short.
const LEGEND_LABEL_CHARS = 24;

const PRIMARY = "var(--foreground)";
const SECONDARY = "var(--muted-foreground)";

function Swatch({ dashed, dotted }: { dashed?: boolean; dotted?: boolean }) {
  return (
    <svg
      aria-hidden="true"
      focusable="false"
      width="16"
      height="4"
      viewBox="0 0 16 4"
      className="shrink-0"
    >
      <line
        x1="0"
        x2="16"
        y1="2"
        y2="2"
        stroke={dashed || dotted ? SECONDARY : PRIMARY}
        strokeWidth={1.5}
        strokeDasharray={dashed ? "4 4" : dotted ? "1 3" : undefined}
        data-dashed={dashed ? "true" : undefined}
        data-dotted={dotted ? "true" : undefined}
      />
    </svg>
  );
}

// LegendLabel is a series name. A long one is cut with an ellipsis and its full name opens
// in a Tooltip; only then is it a tab stop, so a legend of short words adds none.
function LegendLabel({ label }: { label: string }) {
  const long = label.length > LEGEND_LABEL_CHARS;
  const text = (
    <span className="max-w-48 truncate" tabIndex={long ? 0 : undefined}>
      {label}
    </span>
  );
  if (!long) return text;
  return (
    <Tooltip>
      <TooltipTrigger asChild>{text}</TooltipTrigger>
      <TooltipContent>{label}</TooltipContent>
    </Tooltip>
  );
}

// StatsChart is one chart of the stats panel: an area chart of 192 px, the y-axis from 0,
// primary series in the foreground colour and the second dashed in the muted one, no
// animation, and a summary label that gives the same numbers in words. It is the only file
// that imports recharts, and the panel loads it lazily.
export function StatsChart({
  title,
  points,
  series,
  format,
  floor,
  limit,
  limitLabel,
}: StatsChartProps) {
  const t = useT();

  const real = points.filter((p) => !isGap(p));
  const last = real[real.length - 1];
  const hasData = real.some((p) => series.some((s) => p[s.key] !== null));
  const join = (value: (s: ChartSeries) => number | null): string =>
    series.length === 1
      ? format(value(series[0] as ChartSeries))
      : series.map((s) => `${s.label} ${format(value(s))}`).join(", ");
  const summary = hasData
    ? t("stats.charts.summary", {
        title,
        now: join((s) => (last ? last[s.key] : null)),
        high: join((s) => {
          const seen = real.map((p) => p[s.key]).filter((v): v is number => v !== null);
          return seen.length ? Math.max(...seen) : null;
        }),
      })
    : t("stats.charts.summaryWaiting", { title });

  const config: ChartConfig = Object.fromEntries(
    series.map((s) => [s.key, { label: s.label, color: s.dashed ? SECONDARY : PRIMARY }]),
  );
  const top = (dataMax: number): number => Math.max(floor ?? 0, limit ?? 0, dataMax);
  const clock = new Intl.DateTimeFormat(undefined, {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });

  return (
    <figure className="flex flex-col gap-2 rounded-xl bg-card p-4 text-card-foreground tabular-nums ring-1 ring-foreground/10">
      <figcaption className="text-sm font-semibold">{title}</figcaption>
      <ul
        aria-label={title}
        className="flex flex-wrap gap-x-4 gap-y-1 text-xs text-muted-foreground"
      >
        {series.map((s) => (
          <li key={s.key} className="flex min-w-0 items-center gap-1">
            <Swatch dashed={s.dashed} />
            <LegendLabel label={s.label} />
          </li>
        ))}
        {limit && limitLabel ? (
          <li className="flex min-w-0 items-center gap-1">
            <Swatch dotted />
            <LegendLabel label={limitLabel} />
          </li>
        ) : null}
      </ul>
      <div role="img" aria-label={summary} className="h-48 w-full">
        <ChartContainer config={config} className="aspect-auto size-full">
          <AreaChart
            data={points}
            accessibilityLayer
            margin={{ top: 8, right: 8, bottom: 0, left: 0 }}
          >
            <CartesianGrid vertical={false} stroke="var(--border)" />
            <XAxis dataKey="t" type="number" domain={["dataMin", "dataMax"]} hide />
            <YAxis
              width={64}
              domain={[0, top]}
              tickLine={false}
              axisLine={false}
              tickCount={4}
              tickFormatter={(v: number) => format(v)}
            />
            <ChartTooltip
              content={
                <ChartTooltipContent
                  labelFormatter={(_label, payload) => {
                    const t0 = payload?.[0]?.payload?.t;
                    return typeof t0 === "number" ? clock.format(t0) : "";
                  }}
                  formatter={(value, name) => (
                    <div className="flex w-full items-center justify-between gap-4">
                      <span className="text-muted-foreground">{name}</span>
                      <span className="font-medium tabular-nums">{format(Number(value))}</span>
                    </div>
                  )}
                />
              }
            />
            {limit ? (
              <ReferenceLine
                y={limit}
                stroke={SECONDARY}
                strokeDasharray="1 3"
                ifOverflow="extendDomain"
                label={
                  limitLabel
                    ? {
                        value: limitLabel,
                        position: "insideTopRight",
                        fill: SECONDARY,
                        fontSize: 11,
                      }
                    : undefined
                }
              />
            ) : null}
            {series.map((s) => (
              <Area
                key={s.key}
                dataKey={s.key}
                name={s.label}
                type="linear"
                stroke={s.dashed ? SECONDARY : PRIMARY}
                strokeWidth={1.5}
                strokeDasharray={s.dashed ? "4 4" : undefined}
                fill={s.dashed ? "none" : PRIMARY}
                fillOpacity={s.dashed ? 0 : 0.08}
                dot={false}
                activeDot={{ r: 3 }}
                connectNulls={false}
                // Charts re-render every two seconds; an animation would never finish, and
                // reduced motion asks for none (UI-SPEC "Charts").
                isAnimationActive={false}
              />
            ))}
          </AreaChart>
        </ChartContainer>
      </div>
      <div aria-hidden="true" className="flex justify-between text-xs text-muted-foreground">
        <span>{t("stats.charts.ago")}</span>
        <span>{t("stats.charts.now")}</span>
      </div>
    </figure>
  );
}
