// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Link } from "@tanstack/react-router";
import { CircleCheck, CircleX, Info, Lock, ShieldCheck, TriangleAlert } from "lucide-react";
import type { ReactNode } from "react";

import {
  JanitorRequestError,
  type JanitorRun,
  type JanitorRunItem,
  useJanitorRun,
} from "@/api/janitor";
import { NoPermission } from "@/components/full-page-state";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import {
  countParts,
  durationText,
  formatSize,
  reasonText,
  type Translate,
} from "@/features/janitor/preview-summary";
import { TruncatedText } from "@/features/resources/resource-table";
import { useEnvName } from "@/features/resources/use-env-name";
import { usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { formatAbsolute } from "@/lib/format";
import { cn } from "@/lib/utils";

const ROW = "h-9 max-md:h-11";
const CELL = "px-3 py-0";

// The icon of a run's state: always beside a word, never colour alone (UI-SPEC "Color").
function StateIcon({ state }: { state: JanitorRun["state"] }) {
  switch (state) {
    case "running":
      return <Spinner aria-hidden="true" />;
    case "waiting":
      return <Lock aria-hidden="true" className="size-4 text-status-warning-text" />;
    case "completed":
      return <CircleCheck aria-hidden="true" className="size-4 text-status-online-text" />;
    case "partial":
      return <TriangleAlert aria-hidden="true" className="size-4 text-status-warning-text" />;
    case "skipped":
      return <Info aria-hidden="true" className="size-4 text-muted-foreground" />;
    default:
      return <CircleX aria-hidden="true" className="size-4 text-status-offline-text" />;
  }
}

function OutcomeIcon({ outcome }: { outcome: JanitorRunItem["outcome"] }) {
  switch (outcome) {
    case "removed":
      return <CircleCheck aria-hidden="true" className="size-4 text-status-online-text" />;
    case "refused_in_use":
      return <Info aria-hidden="true" className="size-4 text-muted-foreground" />;
    case "skipped_protected":
      return <ShieldCheck aria-hidden="true" className="size-4 text-muted-foreground" />;
    case "skipped_node_offline":
      return <TriangleAlert aria-hidden="true" className="size-4 text-status-warning-text" />;
    default:
      return <CircleX aria-hidden="true" className="size-4 text-status-offline-text" />;
  }
}

// itemReason is why an item ended as it did. A code names a refusal or a failure of the attempt, and is
// worded with the item's own name and node; with none, the reason the run selected it for is shown.
// The run item's name is whatever Docker called it, and is placed as text.
export function itemReason(item: JanitorRunItem, t: Translate): string {
  if (item.code) {
    const key = `janitor.run.itemReason.${item.code}`;
    const words = t(key, { name: item.name, node: item.node });
    if (words !== key) return words;
  }
  return reasonText(item.reason, t);
}

// RunPage is one run of the janitor, live or recorded (D-14): what started it, how it ended, what it
// counted, and every item it attempted with the outcome in words. A run that is waiting or working is
// read again every two seconds. It reads a run of this environment only; a run the history limit
// removed is a not-found page, not an error.
export function RunPage({ envId, runId }: { envId: string; runId: string }) {
  const t = useT();
  const readable = usePermission("environments.read", envId);
  const envName = useEnvName(envId);
  const query = useJanitorRun(envId, runId);
  if (!readable) return <NoPermission />;
  const run = query.data;
  const gone = query.error instanceof JanitorRequestError && query.error.notFound;

  if (gone) {
    return (
      <div className="mx-auto flex w-full max-w-7xl flex-col gap-4">
        <Crumb envId={envId} label={null} />
        <h1 className="text-2xl font-semibold">{t("janitor.run.notFound.title")}</h1>
        <Button asChild variant="outline" className="w-fit max-md:h-11">
          <Link to="/env/$envId/janitor" params={{ envId }} hash="history">
            {t("janitor.run.notFound.link")}
          </Link>
        </Button>
      </div>
    );
  }
  if (query.isError && !run) {
    return (
      <div className="mx-auto flex w-full max-w-7xl flex-col gap-4">
        <Crumb envId={envId} label={null} />
        <Alert variant="destructive">
          <AlertTitle>{t("janitor.run.error.title")}</AlertTitle>
          <AlertDescription className="flex flex-wrap items-center gap-3">
            <span>{t("janitor.run.error.body")}</span>
            <Button
              variant="outline"
              size="sm"
              disabled={query.isFetching}
              onClick={() => void query.refetch()}
            >
              {query.isFetching ? <Spinner aria-hidden="true" /> : null}
              {t("janitor.run.error.retry")}
            </Button>
          </AlertDescription>
        </Alert>
      </div>
    );
  }
  if (!run) {
    return (
      <div className="mx-auto flex w-full max-w-7xl flex-col gap-4" aria-busy="true">
        <Crumb envId={envId} label={null} />
        <h1 className="text-2xl font-semibold">{t("janitor.run.title", { env: envName })}</h1>
        <Skeleton className="h-4 w-80" />
        <Skeleton className="h-32 w-full" />
      </div>
    );
  }
  return <RunBody envId={envId} envName={envName} run={run} />;
}

function Crumb({ envId, label }: { envId: string; label: ReactNode }) {
  const t = useT();
  return (
    <nav aria-label={t("janitor.run.breadcrumb.label")}>
      <ol className="flex flex-wrap items-center gap-x-2 text-xs text-muted-foreground [&>li:not(:last-child)]:after:ms-2 [&>li:not(:last-child)]:after:content-['/']">
        <li>
          <Link
            to="/env/$envId/janitor"
            params={{ envId }}
            className="rounded-sm text-accent-ink hover:underline"
          >
            {t("janitor.run.breadcrumb.janitor")}
          </Link>
        </li>
        {label ? <li className="min-w-0">{label}</li> : null}
      </ol>
    </nav>
  );
}

function RunBody({ envId, envName, run }: { envId: string; envName: string; run: JanitorRun }) {
  const t = useT();
  const items = run.items ?? [];
  const counts = run.counts;
  const removed = countParts(
    {
      containers: counts.removedContainers,
      networks: counts.removedNetworks,
      images: counts.removedImages,
    },
    t,
  );
  const removedTotal = counts.removedContainers + counts.removedNetworks + counts.removedImages;
  const started = formatAbsolute(run.startedAt);
  const seconds = run.finishedAt
    ? Math.max(0, (Date.parse(run.finishedAt) - Date.parse(run.startedAt)) / 1000)
    : undefined;
  const showNode = items.some((i) => i.node !== "");
  const reasonWords = run.reason ? t(`janitor.run.reason.${run.reason}`) : "";
  const attempted = items.length;

  return (
    <div className="mx-auto flex w-full max-w-7xl flex-col gap-4">
      <Crumb envId={envId} label={t("janitor.run.breadcrumb.run", { time: started })} />
      <header className="flex flex-col gap-1">
        <h1 className="flex flex-wrap items-center gap-x-3 gap-y-1 text-2xl font-semibold">
          <span className="break-all">{t("janitor.run.title", { env: envName })}</span>
          <span className="inline-flex items-center gap-1.5 text-base">
            <StateIcon state={run.state} />
            {t(`janitor.run.state.${run.state}`)}
          </span>
        </h1>
        <p className="text-xs text-muted-foreground">
          {run.trigger === "schedule"
            ? t("janitor.run.subtitle.schedule", { started })
            : t("janitor.run.subtitle.manual", { user: run.actorName, started })}{" "}
          {run.finishedAt && seconds !== undefined
            ? t("janitor.run.subtitle.finished", {
                finished: formatAbsolute(run.finishedAt),
                duration: durationText(seconds, t),
              })
            : null}
        </p>
      </header>

      {run.state === "waiting" ? (
        <Alert variant="warning" role="status">
          <Lock aria-hidden="true" />
          <AlertDescription className="text-foreground">
            {t("janitor.run.waiting")}
          </AlertDescription>
        </Alert>
      ) : null}

      <p className="text-sm">
        {removed.length > 0
          ? t("janitor.run.summary.removed", { parts: removed.join(", ") })
          : t("janitor.run.summary.removedNone")}{" "}
        {t("janitor.run.summary.kept", { count: counts.kept })}{" "}
        {t("janitor.run.summary.failed", { count: counts.failed })}
      </p>
      <p className="text-sm">
        <span className="text-xs font-semibold text-muted-foreground">
          {t("janitor.run.summary.space")}
        </span>{" "}
        <span className="tabular-nums">
          {run.estBytes === null
            ? t("janitor.run.summary.spaceUnknown")
            : t("janitor.counts.size", { size: formatSize(run.estBytes, t) })}
        </span>
      </p>

      {run.state === "partial" ? (
        <Alert variant="warning" role="status">
          <TriangleAlert aria-hidden="true" />
          <AlertTitle>{t("janitor.run.alert.partial.title")}</AlertTitle>
          <AlertDescription className="text-foreground">
            {t("janitor.run.alert.partial.body", {
              removed: removedTotal,
              total: attempted,
              failed: counts.failed,
              kept: counts.kept,
            })}
          </AlertDescription>
        </Alert>
      ) : null}
      {run.state === "skipped" ? (
        <Alert role="status">
          <Info aria-hidden="true" />
          <AlertTitle>{t("janitor.run.alert.skipped.title")}</AlertTitle>
          {reasonWords ? (
            <AlertDescription className="text-foreground">{reasonWords}</AlertDescription>
          ) : null}
        </Alert>
      ) : null}
      {run.state === "failed" ? (
        <Alert variant="destructive" role="alert">
          <CircleX aria-hidden="true" />
          <AlertTitle>{t("janitor.run.alert.failed.title")}</AlertTitle>
          <AlertDescription>
            {t("janitor.run.alert.failed.body", { reason: reasonWords })}
          </AlertDescription>
        </Alert>
      ) : null}

      {items.length === 0 ? (
        <p className="text-sm text-muted-foreground">{t("janitor.run.empty")}</p>
      ) : (
        <>
          <div className="overflow-x-auto rounded-lg border">
            <table className="w-full caption-bottom text-sm">
              <caption className="sr-only">{t("janitor.run.caption")}</caption>
              <TableHeader>
                <TableRow>
                  <TableHead scope="col" className="w-32">
                    {t("janitor.run.columns.type")}
                  </TableHead>
                  <TableHead scope="col" className="min-w-48">
                    {t("janitor.run.columns.name")}
                  </TableHead>
                  {showNode ? (
                    <TableHead scope="col" className="w-36">
                      {t("janitor.run.columns.node")}
                    </TableHead>
                  ) : null}
                  <TableHead scope="col" className="min-w-64">
                    {t("janitor.run.columns.reason")}
                  </TableHead>
                  <TableHead scope="col" className="w-48">
                    {t("janitor.run.columns.outcome")}
                  </TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {items.map((item) => (
                  <TableRow key={item.seq} className={ROW}>
                    <TableCell className={cn(CELL, "w-32")}>
                      {t(`janitor.type.${item.type}`)}
                    </TableCell>
                    <TableCell className={cn(CELL, "min-w-48")}>
                      <TruncatedText mono>{item.name}</TruncatedText>
                    </TableCell>
                    {showNode ? (
                      <TableCell className={cn(CELL, "w-36")}>
                        <TruncatedText mono>{item.node || "—"}</TruncatedText>
                      </TableCell>
                    ) : null}
                    <TableCell className={cn(CELL, "min-w-64 whitespace-normal")}>
                      {itemReason(item, t) || "—"}
                    </TableCell>
                    <TableCell className={cn(CELL, "w-48")}>
                      <span className="inline-flex items-center gap-1.5">
                        <OutcomeIcon outcome={item.outcome} />
                        {t(`janitor.run.outcome.${item.outcome}`)}
                      </span>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </table>
          </div>
          <p className="text-xs text-muted-foreground">
            {t("janitor.run.items", { count: items.length })}
          </p>
        </>
      )}
    </div>
  );
}
