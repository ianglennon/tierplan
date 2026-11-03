// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Link } from "@tanstack/react-router";

import {
  HISTORY_MAX,
  HISTORY_STEP,
  type JanitorRunSummary,
  type JanitorStatus,
  useJanitorRuns,
} from "@/api/janitor";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { durationText, formatSize } from "@/features/janitor/preview-summary";
import { StateIcon } from "@/features/janitor/run-state-icon";
import { formatServerTime, formatStarted, removedParts } from "@/features/janitor/status-text";
import { TruncatedText } from "@/features/resources/resource-table";
import { useT } from "@/i18n/use-t";

type Translate = (key: string, options?: Record<string, unknown>) => string;

// What the Removed cell says: the parts a run removed, or why it removed nothing. A run that is
// still waiting or working has not removed anything yet and shows a dash.
function removedCell(run: JanitorRunSummary, t: Translate): string {
  if (run.state === "waiting" || run.state === "running") return t("janitorStatus.history.none");
  const parts = removedParts(run, t);
  if (parts.length > 0) return parts.join(", ");
  if (run.reason === "failed" && run.state === "failed" && run.jobId === "") {
    // The hub refused the job, so the run never began (too many jobs were running).
    return t("janitorStatus.history.neverStarted");
  }
  if (run.reason) return t(`janitorRun.reason.${run.reason}`);
  return t("janitorStatus.history.removedNone");
}

function RunRow({ envId, run }: { envId: string; run: JanitorRunSummary }) {
  const t = useT();
  const started = formatStarted(run.startedAt);
  return (
    <TableRow className="h-9 max-md:h-11">
      <TableCell className="px-3 py-0 whitespace-nowrap">
        <Link
          to="/env/$envId/janitor/runs/$runId"
          params={{ envId, runId: run.id }}
          aria-label={t("janitorStatus.history.openRun", { time: started })}
          className="rounded-sm text-accent-ink hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none max-md:inline-flex max-md:min-h-11 max-md:items-center"
        >
          {started}
        </Link>
      </TableCell>
      <TableCell className="px-3 py-0 whitespace-nowrap">
        {run.trigger === "schedule" ? (
          t("janitorStatus.history.trigger.schedule")
        ) : (
          <>
            {t("janitorStatus.history.trigger.manual")}{" "}
            <span className="text-xs text-muted-foreground">
              {t("janitorStatus.history.trigger.by", { name: run.actorName })}
            </span>
          </>
        )}
      </TableCell>
      <TableCell className="px-3 py-0 whitespace-nowrap">
        <span className="inline-flex items-center gap-1.5">
          <StateIcon state={run.state} />
          {t(`janitorRun.state.${run.state}`)}
        </span>
      </TableCell>
      <TableCell className="px-3 py-0">
        <TruncatedText>{removedCell(run, t)}</TruncatedText>
      </TableCell>
      <TableCell className="px-3 py-0 whitespace-nowrap tabular-nums">
        {run.estBytes === null
          ? t("janitorStatus.history.none")
          : t("janitor.counts.size", { size: formatSize(run.estBytes, t) })}
      </TableCell>
      <TableCell className="px-3 py-0 whitespace-nowrap tabular-nums">
        {run.durationSeconds === null
          ? t("janitorStatus.history.none")
          : durationText(run.durationSeconds, t)}
      </TableCell>
    </TableRow>
  );
}

interface RunHistoryCardProps {
  envId: string;
  // How many runs the page asks for: 25 to start with, up to 100. It lives in the address.
  limit: number;
  onLimitChange: (limit: number) => void;
  // The janitor's status, for the retention in the caption and the first run in the empty state.
  status?: JanitorStatus;
}

// RunHistoryCard lists every run of the environment, newest first (D-16, JAN-10): when it started,
// what started it, how it ended, what it removed and freed, and how long it took. Each row has one
// link, the Started cell, which opens the run. It shows 25 runs and "Show 25 more" asks for 25 more,
// up to 100, through the page's limit. The title and caption are drawn at once; the rows are
// skeletons until the first answer.
export function RunHistoryCard({ envId, limit, onLimitChange, status }: RunHistoryCardProps) {
  const t = useT();
  const query = useJanitorRuns(envId, limit);
  const runs = query.data?.runs ?? [];
  const more = limit < HISTORY_MAX && (query.data?.total ?? 0) > runs.length;

  return (
    <section id="history" aria-label={t("janitorStatus.history.title")} className="scroll-mt-4">
      <Card>
        <CardHeader>
          <CardTitle>{t("janitorStatus.history.title")}</CardTitle>
          {status ? (
            <CardDescription>
              {t("janitorStatus.history.caption", {
                days: status.retention.days,
                runs: status.retention.runs,
              })}
            </CardDescription>
          ) : (
            <Skeleton className="h-4 w-96 max-w-full" aria-hidden="true" />
          )}
        </CardHeader>
        <CardContent className="flex flex-col gap-3">
          {query.isPending ? (
            <div
              role="status"
              aria-label={t("janitorStatus.history.loading")}
              className="flex flex-col"
            >
              {[0, 1, 2, 3, 4, 5].map((i) => (
                <Skeleton key={i} className="my-0.5 h-8 w-full" aria-hidden="true" />
              ))}
            </div>
          ) : query.isError && !query.data ? (
            <Alert variant="destructive">
              <AlertTitle>{t("janitor.page.error.title")}</AlertTitle>
              <AlertDescription className="flex flex-wrap items-center gap-3">
                <span>{t("janitor.page.error.body")}</span>
                <Button
                  variant="outline"
                  size="sm"
                  disabled={query.isFetching}
                  onClick={() => void query.refetch()}
                >
                  {query.isFetching ? <Spinner aria-hidden="true" /> : null}
                  {t("janitor.page.error.retry")}
                </Button>
              </AlertDescription>
            </Alert>
          ) : runs.length === 0 ? (
            <div className="flex flex-col gap-1">
              <p className="text-sm font-semibold">{t("janitorStatus.history.empty.title")}</p>
              <p className="text-sm text-muted-foreground">
                {!status
                  ? t("janitorStatus.history.empty.bodyNoStatus")
                  : status.nextRunAt
                    ? t("janitorStatus.history.empty.body", {
                        when: formatServerTime(status.nextRunAt, status.timezone),
                      })
                    : t("janitorStatus.history.empty.bodyOff")}
              </p>
            </div>
          ) : (
            <>
              <div className="overflow-x-auto rounded-lg border">
                <table className="w-full caption-bottom text-sm">
                  <caption className="sr-only">{t("janitorStatus.history.tableCaption")}</caption>
                  <TableHeader>
                    <TableRow>
                      <TableHead scope="col" className="w-44">
                        {t("janitorStatus.history.columns.started")}
                      </TableHead>
                      <TableHead scope="col" className="w-40">
                        {t("janitorStatus.history.columns.trigger")}
                      </TableHead>
                      <TableHead scope="col" className="w-36">
                        {t("janitorStatus.history.columns.outcome")}
                      </TableHead>
                      <TableHead scope="col" className="min-w-48">
                        {t("janitorStatus.history.columns.removed")}
                      </TableHead>
                      <TableHead scope="col" className="w-28">
                        {t("janitorStatus.history.columns.space")}
                      </TableHead>
                      <TableHead scope="col" className="w-28">
                        {t("janitorStatus.history.columns.duration")}
                      </TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {runs.map((run) => (
                      <RunRow key={run.id} envId={envId} run={run} />
                    ))}
                  </TableBody>
                </table>
              </div>
              <div className="flex flex-wrap items-center justify-between gap-2">
                <p className="text-xs text-muted-foreground">
                  {t("janitorStatus.history.runs", { count: runs.length })}
                </p>
                {more ? (
                  <Button
                    type="button"
                    variant="ghost"
                    size="sm"
                    className="max-md:h-11"
                    disabled={query.isPlaceholderData}
                    onClick={() => onLimitChange(Math.min(limit + HISTORY_STEP, HISTORY_MAX))}
                  >
                    {query.isPlaceholderData ? <Spinner aria-hidden="true" /> : null}
                    {t("janitorStatus.history.showMore")}
                  </Button>
                ) : null}
              </div>
            </>
          )}
        </CardContent>
      </Card>
    </section>
  );
}
