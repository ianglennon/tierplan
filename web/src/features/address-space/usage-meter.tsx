// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { CircleX, TriangleAlert } from "lucide-react";

import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
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
  // Why an unknown meter could not be measured, shown in a tooltip on "Not measured".
  notMeasuredHint?: string;
  // False where the state has a column of its own: the icon and the state word are then left off the
  // meter. The meter's spoken value still names the state.
  showState?: boolean;
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
  notMeasuredHint,
  showState = true,
}: UsageMeterProps) {
  const t = useT();
  const state = givenState ?? meterState(used, capacity);
  const track = variant === "full" ? "h-2 flex-1" : "h-1.5 w-24 shrink-0";
  if (state === "unknown") {
    return (
      <div className="flex min-w-0 items-center gap-3">
        <div aria-hidden="true" className={cn("rounded-full bg-muted", track)} />
        <span className="shrink-0 text-xs text-muted-foreground">
          {t("addressSpace.states.missing")}
        </span>
        {notMeasuredHint ? (
          <Tooltip>
            <TooltipTrigger asChild>
              <span
                // biome-ignore lint/a11y/noNoninteractiveTabindex: the tooltip must open on keyboard focus
                tabIndex={0}
                className="shrink-0 rounded-sm text-xs font-semibold text-muted-foreground outline-none focus-visible:ring-3 focus-visible:ring-ring/50"
              >
                {t("addressSpace.states.notMeasured")}
              </span>
            </TooltipTrigger>
            <TooltipContent className="max-w-80">{notMeasuredHint}</TooltipContent>
          </Tooltip>
        ) : (
          <span className="shrink-0 text-xs font-semibold text-muted-foreground">
            {t("addressSpace.states.notMeasured")}
          </span>
        )}
      </div>
    );
  }
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
        className={cn("relative overflow-hidden rounded-full bg-muted", track)}
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
      {showState && state === "getting_full" ? (
        <span className="flex shrink-0 items-center gap-1 text-xs font-semibold text-status-warning-text">
          <TriangleAlert aria-hidden="true" className="size-4" />
          {t("addressSpace.states.gettingFull")}
        </span>
      ) : null}
      {showState && state === "almost_full" ? (
        <span className="flex shrink-0 items-center gap-1 text-xs font-semibold text-status-offline-text">
          <CircleX aria-hidden="true" className="size-4" />
          {t("addressSpace.states.almostFull")}
        </span>
      ) : null}
    </div>
  );
}
