// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { CircleX, Rocket } from "lucide-react";
import { useEffect, useRef, useState } from "react";

import { type StackJob, stackQuery } from "@/api/stacks";
import { LiveChip } from "@/components/live-chip";
import { useNow } from "@/components/relative-time";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@/components/ui/empty";
import { Progress } from "@/components/ui/progress";
import { Skeleton } from "@/components/ui/skeleton";
import { useEnvName } from "@/features/resources/use-env-name";
import { DeployOutput } from "@/features/stack-deploy/deploy-output";
import { DeployResult, servicesOf } from "@/features/stack-deploy/deploy-result";
import { DeployStepList } from "@/features/stack-deploy/deploy-step-list";
import { useStackJob } from "@/features/stack-deploy/stack-job-stream";
import { useT } from "@/i18n/use-t";
import { formatRelative } from "@/lib/format";

interface DeployConsoleProps {
  envId: string;
  stackId: string;
  jobId: string;
}

// formatElapsed words a span in seconds as "42 s", "3 min 5 s" or "1 h 2 min".
function formatElapsed(ms: number, t: (key: string, options?: Record<string, unknown>) => string) {
  const total = Math.max(0, Math.floor(ms / 1000));
  if (total < 60) return t("stackDeploy.elapsed.seconds", { count: total });
  const minutes = Math.floor(total / 60);
  if (minutes < 60) {
    return t("stackDeploy.elapsed.minutes", { minutes, seconds: total % 60 });
  }
  return t("stackDeploy.elapsed.hours", { hours: Math.floor(minutes / 60), minutes: minutes % 60 });
}

// useSecond is the current time, advancing once a second while on is true. It reads nothing aloud:
// the element that shows it is aria-live off.
function useSecond(on: boolean): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!on) return;
    setNow(Date.now());
    const id = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(id);
  }, [on]);
  return now;
}

function Elapsed({ job }: { job: StackJob }) {
  const t = useT();
  const running = job.state === "running";
  const now = useSecond(running);
  const start = Date.parse(job.createdAt);
  if (Number.isNaN(start)) return null;
  const end = running ? now : Date.parse(job.finishedAt);
  if (Number.isNaN(end)) return null;
  return (
    <span aria-live="off">
      {t(running ? "stackDeploy.elapsedRunning" : "stackDeploy.elapsedDone", {
        elapsed: formatElapsed(end - start, t),
      })}
    </span>
  );
}

// DeployProgress is the one bar of the console: how many of the services the deploy has taken to
// the swarm, with the count beside it. Its movement is not announced.
function DeployProgress({ job }: { job: StackJob }) {
  const t = useT();
  const rows = servicesOf(job);
  const done = rows.filter((r) => r.state === "done").length;
  const value =
    rows.length === 0 ? (job.state === "done" ? 100 : 0) : Math.round((done / rows.length) * 100);
  return (
    <div className="flex flex-col gap-1.5">
      <Progress value={value} aria-label={t("stackDeploy.progress.label")} />
      <p aria-live="off" className="text-sm text-muted-foreground">
        {t("stackDeploy.progress.text", { done, count: rows.length })}
      </p>
    </div>
  );
}

// outcomesOf are the words the console speaks when a job moves from one state to the next: a service
// that runs, a service that failed and why, and a deploy that completed or was cancelled. Working
// and waiting are not outcomes and are never spoken. A deploy that failed is not spoken here: its
// alert announces itself, once.
export function outcomesOf(
  t: (key: string, options?: Record<string, unknown>) => string,
  before: { items: ReadonlyMap<string, string>; state: string },
  job: StackJob,
): string[] {
  const out: string[] = [];
  for (const row of servicesOf(job)) {
    if (before.items.get(row.id) === row.state) continue;
    if (row.state === "done") out.push(t("stackDeploy.announce.running", { service: row.label }));
    else if (row.state === "failed") {
      const reason = row.detail || row.message;
      out.push(t("stackDeploy.announce.failed", { service: row.label, reason }));
    }
  }
  if (before.state === "running" && job.state === "done")
    out.push(t("stackDeploy.announce.complete"));
  if (before.state === "running" && job.state === "cancelled") {
    out.push(t("stackDeploy.announce.cancelled"));
  }
  return out;
}

// Announcer is the one polite region of the console. It speaks outcomes only, each once, as the job
// changes in front of the person; it says nothing about a job as it was when the console opened, and
// it never moves the focus. The bar, the elapsed time and every intermediate state are aria-live off.
function Announcer({ job }: { job: StackJob }) {
  const t = useT();
  const seen = useRef<{ items: Map<string, string>; state: string } | null>(null);
  const [spoken, setSpoken] = useState<string[]>([]);
  useEffect(() => {
    const before = seen.current;
    seen.current = {
      items: new Map((job.items ?? []).map((i) => [i.id, i.state])),
      state: job.state,
    };
    if (!before) return;
    const next = outcomesOf(t, before, job);
    if (next.length > 0) setSpoken((s) => [...s, ...next].slice(-50));
  }, [job, t]);
  return (
    <div data-slot="deploy-announcer" aria-live="polite" className="sr-only">
      {spoken.map((message, i) => (
        // biome-ignore lint/suspicious/noArrayIndexKey: a message is only ever added at the end
        <p key={i}>{message}</p>
      ))}
    </div>
  );
}

// DeployConsole is the live view of a deploy (STK-07, D-13): the title and how long it has run, the
// connection, the overall progress, the steps with a row for each service, and the result. It is a
// view of a job on the server, so leaving never stops the deploy: opening it again shows the job as
// it stands and follows it from there, resuming a dropped stream without losing or repeating an event.
// The title takes the focus when the route opens, and focus never moves on its own after that.
// Every name, label and message is text.
export function DeployConsole({ envId, stackId, jobId }: DeployConsoleProps) {
  const t = useT();
  const envName = useEnvName(envId);
  const stack = useQuery(stackQuery(envId, stackId));
  const follow = useStackJob(envId, stackId, jobId);
  const now = useNow();
  const title = useRef<HTMLHeadingElement>(null);
  const name = stack.data?.name;
  const job = follow.job;

  // Focus moves to the title once, when the route opens and the title is there.
  const titled = name !== undefined && !!job;
  useEffect(() => {
    if (titled) title.current?.focus({ preventScroll: true });
  }, [titled]);

  if (follow.gone) {
    return (
      <Empty className="border">
        <EmptyHeader>
          <EmptyMedia variant="icon">
            <Rocket aria-hidden="true" />
          </EmptyMedia>
          <EmptyTitle role="heading" aria-level={3}>
            {t("stackDeploy.gone.title")}
          </EmptyTitle>
          <EmptyDescription>{t("stackDeploy.gone.body")}</EmptyDescription>
        </EmptyHeader>
        <EmptyContent>
          <Button asChild variant="outline" className="max-md:h-11">
            <Link to="/env/$envId/stacks/$stackId" params={{ envId, stackId }}>
              {t("stackDeploy.openStack")}
            </Link>
          </Button>
        </EmptyContent>
      </Empty>
    );
  }

  if (follow.loadError) {
    return (
      <Alert variant="destructive">
        <CircleX aria-hidden="true" />
        <AlertDescription className="flex flex-wrap items-center gap-3">
          <span>{t("stackDeploy.error.title")}</span>
          <span>{t("stackDeploy.error.body")}</span>
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="max-md:h-11"
            onClick={follow.reload}
          >
            {t("stackDeploy.error.retry")}
          </Button>
        </AlertDescription>
      </Alert>
    );
  }

  if (!job || name === undefined) {
    return (
      <div role="status" aria-label={t("stackDeploy.loading")} className="flex flex-col gap-3">
        <Skeleton className="h-7 w-64" />
        <Skeleton className="h-6 w-full max-w-md" />
        <Skeleton className="h-6 w-full max-w-md" />
        <Skeleton className="h-6 w-full max-w-md" />
      </div>
    );
  }

  const ended = job.state !== "running";
  const chip =
    !ended && !follow.finished && (follow.status === "live" || follow.status === "reconnecting")
      ? follow.status
      : undefined;
  const startedAt = formatRelative(job.createdAt, new Date(now), t);

  return (
    <div className="mx-auto flex w-full max-w-[960px] flex-col gap-6">
      <div className="flex flex-col gap-1">
        <div className="flex flex-wrap items-center justify-between gap-3">
          <h3 ref={title} tabIndex={-1} className="text-xl font-semibold break-all outline-none">
            {t("stackDeploy.title", { name })}
          </h3>
          {chip ? <LiveChip state={chip} /> : null}
        </div>
        <p className="text-sm text-muted-foreground">
          {job.startedBy
            ? t("stackDeploy.started", { time: startedAt, user: job.startedBy })
            : t("stackDeploy.startedAnon", { time: startedAt })}{" "}
          <Elapsed job={job} />
        </p>
      </div>
      <DeployProgress job={job} />
      <DeployStepList items={job.items ?? []} />
      <DeployOutput
        lines={follow.output}
        failed={job.state === "failed" || job.state === "partial"}
      />
      <Announcer job={job} />
      <div className="flex flex-col gap-3">
        <DeployResult job={job} envId={envId} stackId={stackId} name={name} envName={envName} />
      </div>
    </div>
  );
}
