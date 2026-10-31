// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { CircleX, TriangleAlert } from "lucide-react";

import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

export type MeterState = "normal" | "getting_full" | "almost_full" | "unknown";

// The thresholds are decided on the exact ratio used / capacity, never on the rounded percent:
// a shown 70 is always at least amber and a shown 69 is never amber (UI-SPEC "Meter states").
// Whole-number arithmetic keeps 0.7 and 0.9 from being compared as floating-point numbers.
export function meterState(used: number, capacity: number): MeterState {
  if (!(capacity > 0)) return "unknown";
  if (used * 10 >= capacity * 9) return "almost_full";
  if (used * 10 >= capacity * 7) return "getting_full";
  return "normal";
}

// The percent shown: the ratio rounded down to a whole number, so 69.99% reads 69.
export function meterPercent(used: number, capacity: number): number {
  if (!(capacity > 0)) return 0;
  return Math.floor((used * 100) / capacity);
}

interface UsageMeterProps {
  used: number;
  capacity: number;
  // The accessible name: the pool CIDR or the network name.
  label: string;
  unit: "subnets" | "addresses";
  variant?: "full" | "compact";
  // True for a number that cannot be taken as exact (every standalone one).
  estimated?: boolean;
  // The server's state and percent, used when given so the whole page agrees with it.
  state?: MeterState;
  percent?: number;
}

const FILL: Record<MeterState, string> = {
  normal: "bg-foreground/70",
  getting_full: "bg-status-warning",
  almost_full: "bg-status-offline",
  unknown: "",
};

// UsageMeter is one bar with its text: a role="meter" track with ticks at 70% and 90%, the whole
// percent beside it and, from 70%, an icon and the state word. The colour of the fill is
// supplementary; the word and the ticks carry the state.
export function UsageMeter({
  used,
  capacity,
  label,
  unit,
  variant = "full",
  estimated,
  state: givenState,
  percent: givenPercent,
}: UsageMeterProps) {
  const t = useT();
  const state = givenState ?? meterState(used, capacity);
  const percent = givenPercent ?? meterPercent(used, capacity);
  const number = new Intl.NumberFormat();
  const stateWord =
    state === "almost_full"
      ? t("addressSpace.meter.almostFull")
      : state === "getting_full"
        ? t("addressSpace.meter.gettingFull")
        : "";
  const valueText = [
    t("addressSpace.meter.value", {
      used: number.format(used),
      capacity: number.format(capacity),
      unit: t(`addressSpace.meter.unit.${unit}`),
      percent: number.format(percent),
    }),
    stateWord,
    estimated ? t("addressSpace.meter.estimated") : "",
  ]
    .filter(Boolean)
    .join(", ");
  const width = Math.min(100, Math.max(0, percent));
  return (
    <div className="flex min-w-0 items-center gap-3">
      {/* biome-ignore lint/a11y/useSemanticElements: a native meter cannot draw the 70% and 90% ticks or take the state fill colours (UI-SPEC "Meter states") */}
      <div
        role="meter"
        aria-label={label}
        aria-valuemin={0}
        aria-valuemax={capacity}
        aria-valuenow={used}
        aria-valuetext={valueText}
        className={cn(
          "relative overflow-hidden rounded-full bg-muted",
          variant === "full" ? "h-2 flex-1" : "h-1.5 w-24 shrink-0",
        )}
      >
        <div className={cn("h-full", FILL[state])} style={{ width: `${width}%` }} />
        <span
          aria-hidden="true"
          className="absolute inset-y-0 w-px bg-border"
          style={{ left: "70%" }}
        />
        <span
          aria-hidden="true"
          className="absolute inset-y-0 w-px bg-border"
          style={{ left: "90%" }}
        />
      </div>
      <span className="shrink-0 text-xs font-semibold tabular-nums">{number.format(percent)}%</span>
      {state === "getting_full" ? (
        <span className="flex shrink-0 items-center gap-1 text-xs font-semibold text-status-warning-text">
          <TriangleAlert aria-hidden="true" className="size-4" />
          {t("addressSpace.states.gettingFull")}
        </span>
      ) : null}
      {state === "almost_full" ? (
        <span className="flex shrink-0 items-center gap-1 text-xs font-semibold text-status-offline-text">
          <CircleX aria-hidden="true" className="size-4" />
          {t("addressSpace.states.almostFull")}
        </span>
      ) : null}
    </div>
  );
}
