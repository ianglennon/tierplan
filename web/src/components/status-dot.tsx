// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import type { EnvironmentStatus, HealthState } from "@/api/environments";
import { useT } from "@/i18n/use-t";
import { formatClock } from "@/lib/format";
import { cn } from "@/lib/utils";

// The status colours are for health only (UI-SPEC Color, D-33): online is
// green, the states that still work or are mending (degraded, reconnecting,
// unsupported, cluster_changed) are the warning amber, offline and revoked are
// the offline red, pending is a hollow neutral ring because it is expected and
// not a fault, and unknown is a solid neutral grey.
const DOT: Record<HealthState, string> = {
  online: "bg-status-online",
  degraded: "bg-status-warning",
  reconnecting: "bg-status-warning animate-pulse motion-reduce:animate-none",
  unsupported: "bg-status-warning",
  cluster_changed: "bg-status-warning",
  offline: "bg-status-offline",
  revoked: "bg-status-offline",
  pending: "border-2 border-status-unknown bg-transparent",
  unknown: "bg-status-unknown",
};

const TEXT: Record<HealthState, string> = {
  online: "text-status-online-text",
  degraded: "text-status-warning-text",
  reconnecting: "text-status-warning-text",
  unsupported: "text-status-warning-text",
  cluster_changed: "text-status-warning-text",
  offline: "text-status-offline-text",
  revoked: "text-status-offline-text",
  pending: "text-muted-foreground",
  unknown: "text-muted-foreground",
};

interface StatusDotProps {
  state: HealthState;
  // The visible text. When it is left out (a compact place such as the
  // switcher trigger) the dot itself carries the state as its accessible name,
  // so the state is never colour alone.
  label?: string;
  className?: string;
}

// StatusDot is an 8px health dot with its text label (D-54).
export function StatusDot({ state, label, className }: StatusDotProps) {
  const t = useT();
  if (label === undefined) {
    return (
      <span
        role="img"
        aria-label={t(`env.health.${state}`)}
        className={cn("inline-block size-2 shrink-0 rounded-full", DOT[state], className)}
      />
    );
  }
  return (
    <span className={cn("inline-flex min-w-0 items-center gap-1 text-xs", TEXT[state], className)}>
      <span
        aria-hidden="true"
        className={cn("inline-block size-2 shrink-0 rounded-full", DOT[state])}
      />
      <span>{label}</span>
    </span>
  );
}

// useHealthLabel turns an environment's status into the health text for any of
// the nine states: "Online", "Offline since 14:05" (just "Offline" when the
// time is unknown), "Pending", "Reconnecting…" and so on.
export function useHealthLabel(): (status: EnvironmentStatus | undefined) => string {
  const t = useT();
  return (status) => {
    const state = status?.state ?? "unknown";
    if (state === "offline") {
      const time = formatClock(status?.since);
      return time ? t("env.health.offlineSince", { time }) : t("env.health.offline");
    }
    return t(`env.health.${state}`);
  };
}
