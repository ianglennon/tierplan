// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import { Link, useNavigate, useRouterState } from "@tanstack/react-router";
import { BrushCleaning, Download, GitBranch, ListRestart, Rocket, Undo2 } from "lucide-react";
import { useEffect, useRef, useState } from "react";

import { environmentsQuery } from "@/api/environments";
import {
  type JanitorJobSummary,
  type JanitorRun,
  janitorRunQuery,
  useJanitorJobs,
} from "@/api/janitor";
import { type PullSummary, pullsListQuery } from "@/api/pulls";
import { type StackJobSummary, stackJobsListQuery } from "@/api/stacks";
import { RelativeTime } from "@/components/relative-time";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Popover, PopoverContent, PopoverTitle, PopoverTrigger } from "@/components/ui/popover";
import { ScrollArea } from "@/components/ui/scroll-area";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { PullDialog } from "@/features/pulls/pull-dialog";
import { useCancelPullAction } from "@/features/pulls/pull-progress";
import { isFollowed } from "@/features/stacks/follow-operation";
import { useMe } from "@/hooks/use-me";
import { keyExists, useT } from "@/i18n/use-t";
import { toastErrorWithAction, toastSuccessWithAction } from "@/lib/toast";
import { cn } from "@/lib/utils";

// The badge reads "9+" past this many running jobs.
const BADGE_CAP = 9;

// A job of any of the three kinds, as one row of the list.
type Job =
  | { type: "pull"; pull: PullSummary }
  | { type: "stack"; job: StackJobSummary }
  | { type: "janitor"; job: JanitorJobSummary };

const startedOf = (j: Job): string => (j.type === "pull" ? j.pull.createdAt : j.job.startedAt);
const runningOf = (j: Job): boolean =>
  j.type === "pull" ? j.pull.state === "running" : j.job.state === "running";
const idOf = (j: Job): string =>
  j.type === "pull" ? j.pull.id : j.type === "stack" ? j.job.id : j.job.jobId;

// jobsOf merges the three lists: running jobs first, then the newest first within each group.
function jobsOf(
  pulls: PullSummary[],
  stack: StackJobSummary[],
  janitor: JanitorJobSummary[],
): Job[] {
  const all: Job[] = [
    ...pulls.map((pull): Job => ({ type: "pull", pull })),
    ...stack.map((job): Job => ({ type: "stack", job })),
    ...janitor.map((job): Job => ({ type: "janitor", job })),
  ];
  return all.sort((a, b) => {
    if (runningOf(a) !== runningOf(b)) return runningOf(a) ? -1 : 1;
    return Date.parse(startedOf(b)) - Date.parse(startedOf(a));
  });
}

// pullStateText is the words for where a pull stands (UI-SPEC "Pulls indicator"). A running
// pull says how many of its nodes have not finished.
function pullStateKey(p: PullSummary): { key: string; values: Record<string, number> } {
  const values = {
    running: Math.max(p.total - p.done - p.failed, 0),
    count: p.total,
    failed: p.failed,
  };
  switch (p.state) {
    case "running":
      return { key: "jobs.indicator.state.running", values };
    case "done":
      return { key: "jobs.indicator.state.done", values };
    case "partial":
      return {
        key: p.failed > 0 ? "jobs.indicator.state.partialFailed" : "jobs.indicator.state.partial",
        values,
      };
    case "failed":
      return { key: "jobs.indicator.state.failed", values };
    default:
      return { key: "jobs.indicator.state.cancelled", values };
  }
}

// stackStateKey is the words for where a stack operation stands: by what it is (a deploy, an update of
// images, a roll back, a stop, a start, a removal), and for an operation this version has no words for yet, the general ones.
function stackStateKey(job: StackJobSummary): string {
  // A deploy or an update that no person started says so (D-20): Auto-deploying, Auto-deployed.
  if (job.trigger && (job.kind === "deploy" || job.kind === "update")) {
    return `jobs.indicator.stack.auto.${job.state}`;
  }
  const specific = `jobs.indicator.stack.${job.kind}.${job.state}`;
  return keyExists(specific) ? specific : `jobs.indicator.stack.other.${job.state}`;
}

// consoleOf is the path of the console of a stack job, which a person on it needs no toast about.
const consoleOf = (j: StackJobSummary): string =>
  `/env/${j.envId}/stacks/${j.stackId}/deploys/${j.id}`;

// useStackJobToasts tells a person, once, how a stack operation they started ended while they were not
// looking at its console (D-13): Deployed {name}. with Open, or Deploy of {name} failed. with Open
// console. It words each kind as itself (a stop as a stop, a removal as a removal) and an operation this
// version has no words for in general ones, never as a deploy. A removal's Open goes to the
// environment's stack list, since the stack it names is gone. It speaks only of what ends after the page opened: a job that had already ended when the
// list first came is not news. A cancel is the person's own doing and raises nothing. The header is
// mounted for the life of the page, so one set of ids is "once per job per session".
function useStackJobToasts(jobs: StackJobSummary[] | undefined, username: string | undefined) {
  const t = useT();
  const navigate = useNavigate();
  const pathname = useRouterState({ select: (s) => s.location.pathname });
  const seeded = useRef(false);
  const told = useRef(new Set<string>());
  useEffect(() => {
    if (!jobs) return;
    const first = !seeded.current;
    for (const job of jobs) {
      if (job.state === "running" || told.current.has(job.id)) continue;
      if (first) {
        told.current.add(job.id);
        continue;
      }
      // Until the person is known the job waits to be looked at again.
      if (!username) continue;
      told.current.add(job.id);
      if (job.startedBy !== username || job.state === "cancelled") continue;
      if (pathname === consoleOf(job)) continue;
      // An operation this page follows is told by its follower.
      if (isFollowed(job.id)) continue;
      const words = keyExists(`jobs.toast.${job.kind}.done`) ? job.kind : "other";
      if (job.state === "done") {
        toastSuccessWithAction(
          t(`jobs.toast.${words}.done`, { name: job.stackName }),
          t("jobs.toast.open"),
          () =>
            void (job.kind === "remove"
              ? navigate({ to: "/env/$envId/stacks", params: { envId: job.envId } })
              : navigate({
                  to: "/env/$envId/stacks/$stackId",
                  params: { envId: job.envId, stackId: job.stackId },
                })),
        );
      } else {
        toastErrorWithAction(
          t(`jobs.toast.${words}.failed`, { name: job.stackName }),
          t("jobs.toast.openConsole"),
          () =>
            void navigate({
              to: "/env/$envId/stacks/$stackId/deploys/$jobId",
              params: { envId: job.envId, stackId: job.stackId, jobId: job.id },
            }),
        );
      }
    }
    seeded.current = true;
  }, [jobs, username, pathname, navigate, t]);
}

// JobsIndicator (the Pulls indicator of Phase 5, now for every kind of job) is the header button for
// the work the person can follow (D-26): pulls, the deploys, updates and roll backs of stacks, and
// the clean-ups of the janitor, a scheduled one included. It lists the jobs running and those that
// ended in the last ten minutes, running ones first and the newest first, and a click on a pull
// reopens its progress panel while a click on a stack job opens its console and on a clean-up its
// run. A clean-up has no Cancel (UI-SPEC interpretation 6). Both open on the job as it stands: they reconnect to its stream. It is hidden while
// there is nothing to list, and it never changes colour: the count badge is neutral.
export function JobsIndicator() {
  const t = useT();
  const pullsQuery = useQuery(pullsListQuery);
  const stackQuery = useQuery(stackJobsListQuery);
  const janitorQuery = useJanitorJobs();
  const me = useMe();
  useStackJobToasts(stackQuery.data, me.data?.user.username);
  const [open, setOpen] = useState(false);
  const [opened, setOpened] = useState<PullSummary | null>(null);

  const jobs = jobsOf(pullsQuery.data ?? [], stackQuery.data ?? [], janitorQuery.data ?? []);
  const running = jobs.filter(runningOf).length;

  if (jobs.length === 0 && !opened) return null;

  // A list that cannot be read now, though it was: the jobs on it are shown as last seen and say so.
  const pullsStale = pullsQuery.isRefetchError;
  const stackStale = stackQuery.isRefetchError;
  const janitorStale = janitorQuery.isRefetchError;

  return (
    <>
      {jobs.length > 0 ? (
        <Popover open={open} onOpenChange={setOpen}>
          <PopoverTrigger asChild>
            <Button
              variant="ghost"
              size="icon"
              className="relative max-md:size-11"
              aria-label={
                running > 0
                  ? t("jobs.indicator.buttonCount", { count: running })
                  : t("jobs.indicator.button")
              }
            >
              <Download aria-hidden="true" />
              {running > 0 ? (
                <Badge
                  aria-hidden="true"
                  variant="secondary"
                  className="absolute -top-0.5 -right-0.5 h-4 min-w-4 justify-center rounded-full bg-muted px-1 text-[10px] text-foreground"
                >
                  {running > BADGE_CAP ? `${BADGE_CAP}+` : running}
                </Badge>
              ) : null}
            </Button>
          </PopoverTrigger>
          <PopoverContent
            align="end"
            className="w-96"
            onOpenAutoFocus={(event) => {
              event.preventDefault();
              if (event.currentTarget instanceof HTMLElement) event.currentTarget.focus();
            }}
          >
            <PopoverTitle>{t("jobs.indicator.title")}</PopoverTitle>
            {running === 0 ? (
              <p className="text-sm text-muted-foreground">{t("jobs.indicator.empty")}</p>
            ) : null}
            <ScrollArea className="[&_[data-slot=scroll-area-viewport]]:max-h-80 [&_[data-slot=scroll-area-viewport]>div]:block!">
              <ul className="flex flex-col gap-1">
                {jobs.map((j) =>
                  j.type === "pull" ? (
                    <PullRow
                      key={`pull:${idOf(j)}`}
                      pull={j.pull}
                      stale={pullsStale}
                      onOpen={() => {
                        setOpen(false);
                        setOpened(j.pull);
                      }}
                    />
                  ) : j.type === "stack" ? (
                    <StackJobRow
                      key={`stack:${idOf(j)}`}
                      job={j.job}
                      stale={stackStale}
                      onOpen={() => setOpen(false)}
                    />
                  ) : (
                    <JanitorJobRow
                      key={`janitor:${idOf(j)}`}
                      job={j.job}
                      stale={janitorStale}
                      onOpen={() => setOpen(false)}
                    />
                  ),
                )}
              </ul>
              {pullsQuery.isPending || stackQuery.isPending || janitorQuery.isPending ? (
                <LoadingRows />
              ) : null}
              {pullsQuery.isError && !pullsQuery.data ? (
                <LoadError
                  message={t("jobs.indicator.error.pulls")}
                  onRetry={() => void pullsQuery.refetch()}
                />
              ) : null}
              {stackQuery.isError && !stackQuery.data ? (
                <LoadError
                  message={t("jobs.indicator.error.stack")}
                  onRetry={() => void stackQuery.refetch()}
                />
              ) : null}
              {janitorQuery.isError && !janitorQuery.data ? (
                <LoadError
                  message={t("jobs.indicator.error.janitor")}
                  onRetry={() => void janitorQuery.refetch()}
                />
              ) : null}
            </ScrollArea>
          </PopoverContent>
        </Popover>
      ) : null}
      {opened ? (
        <PullDialog
          key={opened.id}
          envId={opened.envId}
          open
          onOpenChange={(next) => {
            if (!next) setOpened(null);
          }}
          hostName={opened.envName}
          swarm={false}
          jobId={opened.id}
          jobRef={opened.ref}
        />
      ) : null}
    </>
  );
}

// LoadingRows stand in for the jobs of a list that has not come yet.
function LoadingRows() {
  const t = useT();
  return (
    <div
      role="status"
      aria-label={t("jobs.indicator.loading")}
      className="flex flex-col gap-2 py-1"
    >
      <Skeleton className="h-10 w-full" />
      <Skeleton className="h-10 w-full" />
    </div>
  );
}

// LoadError says a list could not be read at all, and reads it again on request.
function LoadError({ message, onRetry }: { message: string; onRetry: () => void }) {
  const t = useT();
  return (
    <div className="flex items-center justify-between gap-2 px-2 py-1.5 text-sm">
      <span className="min-w-0 break-words">{message}</span>
      <Button
        type="button"
        variant="outline"
        size="sm"
        className="shrink-0 max-md:h-11"
        onClick={onRetry}
      >
        {t("jobs.indicator.error.retry")}
      </Button>
    </div>
  );
}

function PullRow({
  pull,
  stale,
  onOpen,
}: {
  pull: PullSummary;
  stale: boolean;
  onOpen: () => void;
}) {
  const t = useT();
  const state = pullStateKey(pull);
  const action = useCancelPullAction();
  return (
    <li className="flex min-w-0 items-center gap-2 rounded-md hover:bg-muted/50">
      <Download aria-hidden="true" className="ms-2 size-4 shrink-0 text-muted-foreground" />
      <button
        type="button"
        onClick={onOpen}
        className={cn(
          "flex min-w-0 flex-1 flex-col gap-0.5 rounded-md px-2 py-1.5 text-left",
          "focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none max-md:min-h-11",
        )}
      >
        <Tooltip>
          <TooltipTrigger asChild>
            <span className="truncate font-mono text-sm">{pull.ref}</span>
          </TooltipTrigger>
          <TooltipContent className="font-mono break-all">{pull.ref}</TooltipContent>
        </Tooltip>
        <span className="truncate text-xs text-muted-foreground">{pull.envName}</span>
        <span className="flex flex-wrap items-center gap-x-2 text-xs">
          <span>
            {stale && pull.state === "running"
              ? t("jobs.indicator.reconnecting")
              : t(state.key, state.values)}
          </span>
          {pull.finishedAt ? (
            <span className="text-muted-foreground">
              <RelativeTime value={pull.finishedAt} />
            </span>
          ) : null}
          {pull.ownerName ? (
            <span className="text-muted-foreground">
              {t("jobs.indicator.by", { name: pull.ownerName })}
            </span>
          ) : null}
        </span>
      </button>
      {pull.state === "running" ? (
        <Button
          type="button"
          variant="ghost"
          size="sm"
          className="shrink-0 max-md:h-11"
          disabled={action.pending}
          aria-label={t("jobs.indicator.cancelLabel", { ref: pull.ref })}
          onClick={() => action.cancel(pull.id)}
        >
          {action.pending ? <Spinner aria-hidden="true" /> : null}
          {t("jobs.indicator.cancel")}
        </Button>
      ) : null}
    </li>
  );
}

// StackJobRow is a deploy, an update or a roll back: the stack, the environment, where it stands and
// when, and Open, which goes to its console. The cancel is in the console, not here. The stack's name
// is text, cut at the row's edge and given whole in a tooltip.
function StackJobRow({
  job,
  stale,
  onOpen,
}: {
  job: StackJobSummary;
  stale: boolean;
  onOpen: () => void;
}) {
  const t = useT();
  const live = job.state === "running";
  return (
    <li className="flex min-w-0 items-center gap-2 rounded-md hover:bg-muted/50">
      {job.trigger && (job.kind === "deploy" || job.kind === "update") ? (
        <GitBranch aria-hidden="true" className="ms-2 size-4 shrink-0 text-muted-foreground" />
      ) : job.kind === "restore" ? (
        <ListRestart aria-hidden="true" className="ms-2 size-4 shrink-0 text-muted-foreground" />
      ) : job.kind === "rollback" ? (
        <Undo2 aria-hidden="true" className="ms-2 size-4 shrink-0 text-muted-foreground" />
      ) : (
        <Rocket aria-hidden="true" className="ms-2 size-4 shrink-0 text-muted-foreground" />
      )}
      <div className="flex min-w-0 flex-1 flex-col gap-0.5 px-2 py-1.5">
        <Tooltip>
          <TooltipTrigger asChild>
            <span className="truncate text-sm">{job.stackName}</span>
          </TooltipTrigger>
          <TooltipContent className="break-all">{job.stackName}</TooltipContent>
        </Tooltip>
        <span className="truncate text-xs text-muted-foreground">{job.envName}</span>
        <span className="flex flex-wrap items-center gap-x-2 text-xs">
          <span>{stale && live ? t("jobs.indicator.reconnecting") : t(stackStateKey(job))}</span>
          <span className="text-muted-foreground">
            <RelativeTime value={job.finishedAt || job.startedAt} />
          </span>
        </span>
      </div>
      <Button asChild variant="ghost" size="sm" className="shrink-0 max-md:h-11">
        <Link
          to="/env/$envId/stacks/$stackId/deploys/$jobId"
          params={{ envId: job.envId, stackId: job.stackId, jobId: job.id }}
          aria-label={t("jobs.indicator.open.label", { name: job.stackName })}
          onClick={onOpen}
        >
          {t("jobs.indicator.open.action")}
        </Link>
      </Button>
    </li>
  );
}

// janitorStateKey is the words for where a clean-up stands, from the run's record and not from the
// job: the job counts its items, so one the engine kept in use ends it failed or partial for a run
// whose record says completed. Until the record is read, a job that has ended says only that it
// finished.
function janitorStateKey(
  job: JanitorJobSummary,
  record: JanitorRun["state"] | undefined,
): { key: string; values: Record<string, number> } {
  const values = { done: job.done, total: job.total };
  if (job.state === "running" || record === "running" || record === "waiting") {
    return {
      key: job.total > 0 ? "jobs.indicator.janitor.running" : "jobs.indicator.janitor.waiting",
      values,
    };
  }
  if (record) return { key: `jobs.indicator.janitor.${record}`, values };
  return { key: "jobs.indicator.janitor.finished", values };
}

// JanitorJobRow is a clean-up of an environment: "Cleaning {environment}", where it stands ("Removing
// 3 of 8", then Cleaned, Partly cleaned, Failed or Skipped), who started it (TierPlan for a scheduled
// run) and Open, which goes to the run. There is no Cancel. The environment's name is text, cut at the
// row's edge and given whole in a tooltip.
function JanitorJobRow({
  job,
  stale,
  onOpen,
}: {
  job: JanitorJobSummary;
  stale: boolean;
  onOpen: () => void;
}) {
  const t = useT();
  const live = job.state === "running";
  const environments = useQuery(environmentsQuery);
  const name =
    environments.data?.items?.find((e) => e.id === job.envId)?.name ??
    t("dashboard.environmentFallback");
  // The record of a clean-up that has ended says how it ended; one that goes is read on its page.
  const record = useQuery({ ...janitorRunQuery(job.envId, job.runId), enabled: !live });
  const state = janitorStateKey(job, record.data?.state);
  const title = t("jobs.indicator.janitor.title", { name });
  return (
    <li className="flex min-w-0 items-center gap-2 rounded-md hover:bg-muted/50">
      <BrushCleaning aria-hidden="true" className="ms-2 size-4 shrink-0 text-muted-foreground" />
      <div className="flex min-w-0 flex-1 flex-col gap-0.5 px-2 py-1.5">
        <Tooltip>
          <TooltipTrigger asChild>
            <span className="truncate text-sm">{title}</span>
          </TooltipTrigger>
          <TooltipContent className="break-all">{title}</TooltipContent>
        </Tooltip>
        <span className="flex flex-wrap items-center gap-x-2 text-xs">
          <span>
            {stale && live ? t("jobs.indicator.reconnecting") : t(state.key, state.values)}
          </span>
          <span className="text-muted-foreground">
            {job.trigger === "schedule"
              ? t("jobs.indicator.janitor.byTierPlan")
              : t("jobs.indicator.by", { name: job.by })}
          </span>
          {live ? null : (
            <span className="text-muted-foreground">
              <RelativeTime value={record.data?.finishedAt ?? job.startedAt} />
            </span>
          )}
        </span>
      </div>
      <Button asChild variant="ghost" size="sm" className="shrink-0 max-md:h-11">
        <Link
          to="/env/$envId/janitor/runs/$runId"
          params={{ envId: job.envId, runId: job.runId }}
          aria-label={t("jobs.indicator.janitor.open", { name })}
          onClick={onOpen}
        >
          {t("jobs.indicator.open.action")}
        </Link>
      </Button>
    </li>
  );
}
