// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useT } from "@/i18n/use-t";

// LiveChip says how a live view is connected. It shows only while the stream
// is connected or retrying: once a view has fallen back to polling, or has not
// connected yet, there is no chip, so it never claims freshness it does not
// have (T-01-93). The pulse is off under prefers-reduced-motion.
export function LiveChip({ state }: { state: "live" | "reconnecting" }) {
  const t = useT();
  const live = state === "live";
  return (
    <output className="inline-flex items-center gap-1.5 rounded-full border px-2 py-0.5 text-xs">
      <span
        aria-hidden="true"
        className={
          live
            ? "inline-block size-2 shrink-0 animate-pulse rounded-full bg-status-online motion-reduce:animate-none"
            : "inline-block size-2 shrink-0 rounded-full bg-status-warning"
        }
      />
      <span className={live ? "text-status-online-text" : "text-status-warning-text"}>
        {live ? t("dashboard.events.live") : t("dashboard.events.reconnecting")}
      </span>
    </output>
  );
}
