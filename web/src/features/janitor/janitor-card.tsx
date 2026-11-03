// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Link } from "@tanstack/react-router";
import { ChevronRight, TriangleAlert } from "lucide-react";
import type { ReactNode } from "react";

import { type JanitorStatus, useJanitorStatus } from "@/api/janitor";
import { RelativeTime } from "@/components/relative-time";
import { StatusDot } from "@/components/status-dot";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { StateIcon } from "@/features/janitor/run-state-icon";
import { stateOf } from "@/features/janitor/status-card";
import { formatServerTime, removedSentence, scheduleSummary } from "@/features/janitor/status-text";
import { usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

function OpenLink({ envId }: { envId: string }) {
  const t = useT();
  return (
    <Button asChild variant="ghost" size="sm" className="-ms-2.5 self-start">
      <Link to="/env/$envId/janitor" params={{ envId }}>
        {t("janitorStatus.card.open")}
        <ChevronRight aria-hidden="true" data-icon="inline-end" />
      </Link>
    </Button>
  );
}

function Shell({ title, busy, children }: { title: string; busy?: boolean; children: ReactNode }) {
  return (
    <section aria-label={title} aria-busy={busy ? "true" : undefined}>
      <Card>
        <CardHeader>
          <CardTitle>{title}</CardTitle>
        </CardHeader>
        <CardContent className="flex flex-col gap-2">{children}</CardContent>
      </Card>
    </section>
  );
}

// Problems is the line under the last run when something did not go as it should: items that failed
// and items skipped because their node was not reporting. Each is a link to the run, not a link that
// holds another link.
function Problems({ envId, status }: { envId: string; status: JanitorStatus }) {
  const t = useT();
  const last = status.lastRun;
  if (!last) return null;
  const lines = [
    last.counts.failed > 0
      ? t("janitorStatus.card.failedItems", { count: last.counts.failed })
      : "",
    last.counts.skippedOffline > 0
      ? t("janitorStatus.card.offline", { count: last.counts.skippedOffline })
      : "",
  ].filter((line) => line !== "");
  return lines.map((line) => (
    <p key={line} className="flex items-start gap-1.5 text-xs text-status-warning-text">
      <TriangleAlert aria-hidden="true" className="mt-px size-3.5 shrink-0" />
      <Link
        to="/env/$envId/janitor/runs/$runId"
        params={{ envId, runId: last.id }}
        className="rounded-sm underline underline-offset-3 focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
      >
        {line}
      </Link>
    </p>
  ));
}

// JanitorCard is the "Janitor" card on the environment dashboard (D-13): whether it is on, what its
// last run did, anything that went wrong in it, and the way to the page. The last run's outcome and
// what it removed are its focal point. It is hidden from someone who may not read the environment, a
// skeleton of the same height stands in while the status is on its way, and a status that cannot be
// read says so and still offers the page.
export function JanitorCard({ envId }: { envId: string }) {
  const t = useT();
  const canRead = usePermission("environments.read", envId);
  const query = useJanitorStatus(envId, canRead);
  if (!canRead) return null;
  const title = t("janitorStatus.card.title");

  if (query.isPending) {
    return (
      <Shell title={title} busy>
        <span className="sr-only">{t("janitorStatus.card.loading")}</span>
        <Skeleton className="h-5 w-40" aria-hidden="true" />
        <Skeleton className="h-5 w-56" aria-hidden="true" />
        <Skeleton className="h-4 w-2/3" aria-hidden="true" />
        <Skeleton className="h-7 w-32" aria-hidden="true" />
      </Shell>
    );
  }

  const status = query.data;
  if (!status) {
    return (
      <Shell title={title}>
        <p className="text-sm">{t("janitorStatus.card.error")}</p>
        <OpenLink envId={envId} />
      </Shell>
    );
  }

  const state = stateOf(status);
  const last = status.lastRun;
  // On names the schedule: "On, Daily at 03:00". A run that goes says what it is doing instead.
  const stateWords =
    state.key === "status.state.on"
      ? t("janitorStatus.card.on", { summary: scheduleSummary(status, t) })
      : state.key === "status.state.off"
        ? t("janitorStatus.card.off")
        : t(`janitorStatus.${state.key}`);

  return (
    <Shell title={title}>
      <StatusDot state={state.dot} label={stateWords} className="text-sm" />
      {last ? (
        <div className="flex flex-col gap-0.5">
          <span className="flex flex-wrap items-center gap-x-2 gap-y-0.5">
            <span className="inline-flex items-center gap-1.5 text-base font-semibold">
              <StateIcon state={last.state} />
              {t(`janitorRun.state.${last.state}`)}
            </span>
            <span className="text-xs text-muted-foreground">
              <RelativeTime value={last.finishedAt ?? last.startedAt} />
            </span>
          </span>
          <span className="text-sm break-words">{removedSentence(last, t)}</span>
        </div>
      ) : (
        <div className="flex flex-col gap-0.5">
          <span className="text-base font-semibold">{t("janitorStatus.status.last.never")}</span>
          {status.nextRunAt ? (
            <span className="text-xs text-muted-foreground">
              {t("janitorStatus.card.firstRun", {
                when: formatServerTime(status.nextRunAt, status.timezone),
              })}
            </span>
          ) : null}
        </div>
      )}
      <Problems envId={envId} status={status} />
      <OpenLink envId={envId} />
    </Shell>
  );
}
