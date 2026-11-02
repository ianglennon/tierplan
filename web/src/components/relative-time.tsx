// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useSyncExternalStore } from "react";

import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { useT } from "@/i18n/use-t";
import { formatAbsolute, formatRelative } from "@/lib/format";

// Relative times refresh every 30 seconds (D-66). One interval serves every
// RelativeTime on the page, so a table of 500 rows does not start 500 timers.
// The interval runs only while something is subscribed.
const REFRESH_MS = 30_000;

let now = Date.now();
let timer: ReturnType<typeof setInterval> | undefined;
const listeners = new Set<() => void>();

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  if (listeners.size === 1) {
    now = Date.now();
    timer = setInterval(() => {
      now = Date.now();
      for (const l of listeners) l();
    }, REFRESH_MS);
  }
  return () => {
    listeners.delete(listener);
    if (listeners.size === 0 && timer !== undefined) {
      clearInterval(timer);
      timer = undefined;
    }
  };
}

// useNow is the current time in milliseconds, advancing every 30 seconds.
export function useNow(): number {
  return useSyncExternalStore(subscribe, () => now);
}

interface RelativeTimeProps {
  // An RFC 3339 time. A missing one shows "—" (UI-SPEC: partial data).
  value?: string;
}

// RelativeTime shows "5 min ago" as a <time> element. The tooltip, on hover
// and on keyboard focus, shows the absolute local time with its zone.
export function RelativeTime({ value }: RelativeTimeProps) {
  const t = useT();
  const current = useNow();
  const text = value ? formatRelative(value, new Date(current), t) : "";
  if (!value || !text) return <span>{t("dashboard.time.missing")}</span>;
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <time
          dateTime={value}
          // biome-ignore lint/a11y/noNoninteractiveTabindex: the tooltip must open on keyboard focus (UI-SPEC D-66)
          tabIndex={0}
          className="rounded-sm focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
        >
          {text}
        </time>
      </TooltipTrigger>
      <TooltipContent>{formatAbsolute(value)}</TooltipContent>
    </Tooltip>
  );
}
