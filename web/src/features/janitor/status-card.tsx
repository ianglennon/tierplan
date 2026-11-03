// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Link } from "@tanstack/react-router";
import { type ReactNode, useId } from "react";

import type { HealthState } from "@/api/environments";
import type { JanitorStatus } from "@/api/janitor";
import { RelativeTime, useNow } from "@/components/relative-time";
import { StatusDot } from "@/components/status-dot";
import { Card, CardContent } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { StateIcon } from "@/features/janitor/run-state-icon";
import {
  formatServerTime,
  formatUntil,
  removedSentence,
  scheduleLine,
} from "@/features/janitor/status-text";
import { useT } from "@/i18n/use-t";

// The one place a part of the card is a caption with its value: a definition list entry, so the
// caption and the value read together.
function Part({ caption, children }: { caption: string; children: ReactNode }) {
  return (
    <div className="flex min-w-0 flex-col gap-1">
      <dt className="text-xs font-semibold text-muted-foreground">{caption}</dt>
      <dd className="flex min-w-0 flex-col gap-1">{children}</dd>
    </div>
  );
}

// The dot and word of the State part: on or off, and what a run that is going is doing.
export function stateOf(status: JanitorStatus): { dot: HealthState; key: string } {
  if (status.active?.state === "running") return { dot: "online", key: "status.state.running" };
  if (status.active?.state === "waiting") return { dot: "degraded", key: "status.state.waiting" };
  return status.enabled
    ? { dot: "online", key: "status.state.on" }
    : { dot: "pending", key: "status.state.off" };
}

interface StatusCardProps {
  envId: string;
  status?: JanitorStatus;
  loading?: boolean;
}

// StatusCard is the first thing the Janitor page says (D-13): whether the janitor is on, what its
// last run did, and when it runs next. Its last run is the page's focal point. A skeleton of the same
// height stands in while the status is on its way.
export function StatusCard({ envId, status, loading }: StatusCardProps) {
  const t = useT();
  const headingId = useId();
  const now = useNow();
  const heading = t("janitorStatus.status.heading");

  if (loading || !status) {
    return (
      <section aria-labelledby={headingId} aria-busy="true">
        <h2 id={headingId} className="sr-only">
          {heading}
        </h2>
        <Card>
          <CardContent className="grid gap-4 md:grid-cols-3">
            <span className="sr-only">{t("janitorStatus.status.loading")}</span>
            {[0, 1, 2].map((i) => (
              <div key={i} className="flex flex-col gap-1.5" aria-hidden="true">
                <Skeleton className="h-3 w-16" />
                <Skeleton className="h-5 w-32" />
                <Skeleton className="h-4 w-48" />
              </div>
            ))}
          </CardContent>
        </Card>
      </section>
    );
  }

  const state = stateOf(status);
  const last = status.lastRun;
  const next = status.nextRunAt;
  return (
    <section aria-labelledby={headingId}>
      <h2 id={headingId} className="sr-only">
        {heading}
      </h2>
      <Card>
        <CardContent>
          <dl className="grid gap-4 md:grid-cols-3">
            <Part caption={t("janitorStatus.status.state.caption")}>
              <StatusDot
                state={state.dot}
                label={t(`janitorStatus.${state.key}`)}
                className="text-base font-semibold"
              />
              <span className="text-xs text-muted-foreground">
                {status.enabled
                  ? scheduleLine(status, t)
                  : t("janitorStatus.status.state.offDetail")}
              </span>
            </Part>

            <Part caption={t("janitorStatus.status.last.caption")}>
              {last ? (
                <>
                  <span className="flex flex-wrap items-center gap-x-2 gap-y-1">
                    <Link
                      to="/env/$envId/janitor/runs/$runId"
                      params={{ envId, runId: last.id }}
                      aria-label={t("janitorStatus.status.last.open", {
                        outcome: t(`janitorRun.state.${last.state}`),
                      })}
                      className="inline-flex items-center gap-1.5 rounded-sm text-base font-semibold hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
                    >
                      <StateIcon state={last.state} />
                      {t(`janitorRun.state.${last.state}`)}
                    </Link>
                    <span className="text-xs text-muted-foreground">
                      <RelativeTime value={last.finishedAt ?? last.startedAt} />
                    </span>
                  </span>
                  <span className="text-sm break-words">{removedSentence(last, t)}</span>
                </>
              ) : (
                <span className="text-base font-semibold">
                  {t("janitorStatus.status.last.never")}
                </span>
              )}
            </Part>

            <Part caption={t("janitorStatus.status.next.caption")}>
              {next ? (
                <>
                  <Tooltip>
                    <TooltipTrigger asChild>
                      <time
                        dateTime={next}
                        // biome-ignore lint/a11y/noNoninteractiveTabindex: the tooltip must open on keyboard focus
                        tabIndex={0}
                        className="w-fit rounded-sm text-base font-semibold focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
                      >
                        {formatServerTime(next, status.timezone)}
                      </time>
                    </TooltipTrigger>
                    <TooltipContent>{t("janitorStatus.status.next.spread")}</TooltipContent>
                  </Tooltip>
                  <span className="text-xs text-muted-foreground">{formatUntil(next, now)}</span>
                </>
              ) : (
                <>
                  <span className="text-base font-semibold">
                    {t("janitorStatus.status.next.none")}
                  </span>
                  <span className="text-xs text-muted-foreground">
                    {status.enabled
                      ? t("janitorStatus.status.next.unusable")
                      : t("janitorStatus.status.next.offDetail")}
                  </span>
                </>
              )}
            </Part>
          </dl>
        </CardContent>
      </Card>
    </section>
  );
}
