// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { EnvironmentStatus, HealthState } from "@/api/environments";
import { useT } from "@/i18n/use-t";
import { formatClock } from "@/lib/format";
import { cn } from "@/lib/utils";

// The status colours are for health only (UI-SPEC Color): online, unsupported
// as the warning amber, offline, and a neutral grey for unknown.
const DOT: Record<HealthState, string> = {
  online: "bg-status-online",
  unsupported: "bg-status-warning",
  offline: "bg-status-offline",
  unknown: "bg-status-unknown",
};

const TEXT: Record<HealthState, string> = {
  online: "text-status-online-text",
  unsupported: "text-status-warning-text",
  offline: "text-status-offline-text",
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

// useHealthLabel turns an environment's status into the health text:
// "Online", "Offline since 14:05" (just "Offline" when the engine never
// answered or the time is unknown), "Unsupported" or "Unknown".
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
