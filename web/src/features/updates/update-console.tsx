// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Link } from "@tanstack/react-router";
import { CircleCheck, CircleX, Rocket, TriangleAlert, Undo2 } from "lucide-react";
import { useEffect, useRef, useState } from "react";

import type { StackJobItem } from "@/api/stacks";
import { type UpdateJobView, useImageUpdateJob } from "@/api/updates";
import { LiveChip } from "@/components/live-chip";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { useNow } from "@/components/relative-time";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
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
import { durationText } from "@/features/janitor/preview-summary";
import { useEnvName } from "@/features/resources/use-env-name";
import { Elapsed } from "@/features/stack-deploy/deploy-console";
import { DeployOutput } from "@/features/stack-deploy/deploy-output";
import { DeployServiceRow } from "@/features/stack-deploy/deploy-service-row";
import { DeployStepList } from "@/features/stack-deploy/deploy-step-list";
import { shortDigest } from "@/features/tags/platform-chips";
import {
  DEFAULT_CONTAINER_WINDOW_SECONDS,
  DEFAULT_SERVICE_WINDOW_SECONDS,
  restoreReasonText,
  rollbackReasonText,
} from "@/features/updates/reasons";
import { UpdateConfirmDialog } from "@/features/updates/update-confirm-dialog";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { formatRelative } from "@/lib/format";

interface UpdateConsoleProps {
  envId: string;
  jobId: string;
}

type Translate = (key: string, options?: Record<string, unknown>) => string;

// The steps of an update of a plain service or container, by the id the server gives them. A container
// is recreated: the new one is created first, the old one is stopped and renamed, the new one starts,
// is waited for (its health when it has a check), and only then is the old one removed.
const STEP_IDS = [
  "check",
  "pull",
  "update",
  "wait",
  "health",
  "create",
  "swap",
  "start",
  "remove_old",
  "rollback",
  "rollback_wait",
] as const;

// How long a replacement container is waited for when the job does not say: the server's default
// window (five minutes). A health check that needs longer extends it on the server, and the job item
// then carries the real window in seconds.
const DEFAULT_HEALTH_WINDOW_SECONDS = 300;

// The line a container update writes when the old container could not be put back (12-22). A container
// update's steps carry no restore step, so this line is how the console tells a rollback that worked
// from one that did not.
const CONTAINER_RESTORE_FAILED = "The old container could not be put back";

// The steps of a container update that come after the old container was stopped and renamed to make way.
// A failure at one of them has always touched the old container, so TierPlan has tried to put it back.
// A failure of the swap itself may or may not have (the server does not say), so it is not read as one.
const AFTER_THE_OLD_ONE_WAS_TOUCHED: readonly string[] = ["start", "health"];

// UpdateOutcome is how an update ended, in the words the console has for it. The job says only that it
// failed; what TierPlan did about it is read from the steps it added (the rollback and its wait), from
// the step that stopped, and, for a container, from the output.
//   rolled_back     the update failed after something changed and the previous version runs again
//   rollback_failed the update failed and the previous version could not be put back
//   pull_failed     the image could not be pulled, so nothing was changed and nothing was rolled back
//   failed          any other failure (a refusal, or someone else changed the workload)
export type UpdateOutcome =
  | "done"
  | "cancelled"
  | "rolled_back"
  | "rollback_failed"
  | "pull_failed"
  | "failed";

const isRestoreStep = (i: StackJobItem) => i.id === "rollback" || i.id === "rollback_wait";

// failedStepOf is the step that stopped the update itself, not the restore that followed it.
export function failedStepOf(job: UpdateJobView): StackJobItem | undefined {
  return (job.items ?? []).find((i) => i.state === "failed" && !isRestoreStep(i));
}

// outcomeOf reads how an ended job ended. A job that is still running has no outcome, and reads as done
// here only so the caller need not check; it checks first.
export function outcomeOf(
  job: UpdateJobView,
  lines: readonly { text: string }[] = [],
): UpdateOutcome {
  if (job.state === "cancelled") return "cancelled";
  const items = job.items ?? [];
  const back = items.find((i) => i.id === "rollback");
  const wait = items.find((i) => i.id === "rollback_wait");
  if (back || wait) {
    // Somebody else changed the service while the update failed, so nothing was put back over their change.
    if (back?.state === "failed" && back.code === "restore_service_changed") return "failed";
    if (back?.state === "failed" || wait?.state === "failed") return "rollback_failed";
    // A recovered job (TierPlan restarted mid-update) has only the restore, and no step that failed.
    if (back?.state === "done" && (!wait || wait.state === "done")) return "rolled_back";
  }
  if (job.state === "done") return "done";
  const stopped = failedStepOf(job);
  if (stopped?.id === "pull") return "pull_failed";
  if (job.kind === "container" && stopped && AFTER_THE_OLD_ONE_WAS_TOUCHED.includes(stopped.id)) {
    return lines.some((l) => l.text.includes(CONTAINER_RESTORE_FAILED))
      ? "rollback_failed"
      : "rolled_back";
  }
  return "failed";
}

// windowOf is how long the wait that failed was allowed to take, in seconds: the health step's own
// window when it carries one, else the server's default for what is being updated.
function windowOf(job: UpdateJobView): number {
  const health = (job.items ?? []).find((i) => i.id === "health");
  if (health && health.unit === "seconds" && health.total > 0) return health.total;
  return job.kind === "container"
    ? DEFAULT_CONTAINER_WINDOW_SECONDS
    : DEFAULT_SERVICE_WINDOW_SECONDS;
}

// The code of why an update failed: the step that stopped, or, for a job that only restores, the
// restart that interrupted the update it was recovering.
function causeOf(job: UpdateJobView): string {
  const stopped = failedStepOf(job);
  if (!stopped) return "server_restarted";
  return stopped.id === "pull" ? "pull_failed" : stopped.code;
}

// stepsOf are the items of the job as the step list shows them: each step in the words of the
// console (the server's English is only a fallback), the pull naming how many nodes it ran on, and a
// closing "Done" that waits until the update has finished.
export function consoleStepsOf(t: Translate, job: UpdateJobView): StackJobItem[] {
  const items = (job.items ?? []).map((it): StackJobItem => {
    if (!(STEP_IDS as readonly string[]).includes(it.id)) return it;
    const nodes = Number(it.result?.nodes);
    const label =
      it.id === "pull" && Number.isFinite(nodes) && it.result?.nodes
        ? t("updates.console.steps.pullNodes", { count: nodes })
        : t(`updates.console.steps.${it.id}`);
    return { ...it, label };
  });
  const ended = job.state !== "running";
  const state: StackJobItem["state"] =
    job.state === "done" ? "done" : ended ? "skipped" : "waiting";
  items.push({
    id: "done",
    kind: "step",
    label: t("updates.console.steps.done"),
    state,
    code: "",
    current: 0,
    detail: "",
    message: "",
    parent: "",
    result: {},
    total: 0,
    unit: "",
  });
  return items;
}

// rowOf is the service the update works on, as one row of the console: waiting until the new tasks are
// asked for, working while they come up (with their count), done once they run and healthy, failed
// when a step did.
export function rowOf(job: UpdateJobView): StackJobItem {
  if (job.kind === "container") return containerRowOf(job);
  const items = job.items ?? [];
  const wait = items.find((i) => i.id === "wait");
  const health = items.find((i) => i.id === "health");
  const update = items.find((i) => i.id === "update");
  const failed = items.some((i) => i.state === "failed");
  let state: StackJobItem["state"] = "waiting";
  if (failed) state = "failed";
  else if (job.state === "cancelled") state = "cancelled";
  else if (wait?.state === "done" && (!health || health.state === "done")) state = "done";
  else if (update && update.state !== "waiting") state = "running";
  return {
    id: `service:${job.workload}`,
    kind: "service",
    label: job.workload,
    state,
    code: "",
    current: wait?.current ?? 0,
    detail: "",
    message: "",
    parent: "",
    result: {},
    total: wait?.total ?? 0,
    unit: wait?.unit ?? "",
  };
}

// containerRowOf is the container being replaced as one row: working once it is pulled and the new one
// is being made, done when the update has finished well, failed when a step did.
function containerRowOf(job: UpdateJobView): StackJobItem {
  const items = job.items ?? [];
  const failed = items.some((i) => i.state === "failed");
  const started = items.some((i) => i.id !== "check" && i.state !== "waiting");
  let state: StackJobItem["state"] = "waiting";
  if (failed) state = "failed";
  else if (job.state === "cancelled") state = "cancelled";
  else if (job.state === "done") state = "done";
  else if (started) state = "running";
  return {
    id: `container:${job.workload}`,
    kind: "service",
    label: job.workload,
    state,
    code: "",
    current: 0,
    detail: "",
    message: "",
    parent: "",
    result: {},
    total: 0,
    unit: "",
  };
}

// healthWindowOf is how long the job says the new container may take to become healthy, in seconds:
// the item's total when it counts seconds, else the server's default.
function healthWindowOf(item: StackJobItem): number {
  return item.unit === "seconds" && item.total > 0 ? item.total : DEFAULT_HEALTH_WINDOW_SECONDS;
}

// healthQuarter is how far through the window the wait is, in whole quarters (0 to 4).
export function healthQuarter(windowSeconds: number, elapsedSeconds: number): number {
  if (windowSeconds <= 0) return 4;
  return Math.min(4, Math.floor((Math.max(0, elapsedSeconds) * 4) / windowSeconds));
}

// HealthWait is the wait for a new container to be healthy: the bar of the time used, and the line
// that says how long is left. The line counts down every second, but is not announced. The bar's
// spoken text, and the polite region, change only as each quarter of the window is reached, so a
// reader hears four things at most, never a countdown (UI-SPEC accessibility contract). The server
// ends the wait itself; this only shows how long it may still take.
export function HealthWait({ name, windowSeconds }: { name: string; windowSeconds: number }) {
  const t = useT();
  const started = useRef(Date.now());
  const [elapsed, setElapsed] = useState(0);
  const [spoken, setSpoken] = useState<string[]>([]);
  useEffect(() => {
    const timer = setInterval(() => {
      setElapsed(Math.floor((Date.now() - started.current) / 1000));
    }, 1000);
    return () => clearInterval(timer);
  }, []);
  const quarter = healthQuarter(windowSeconds, elapsed);
  const left = Math.max(0, windowSeconds - elapsed);
  // What the bar says is what was left when its quarter began, so it holds still between quarters.
  const atQuarter = Math.ceil((windowSeconds * (4 - quarter)) / 4);
  const valueText = t("updates.console.healthWait.valueText", {
    name,
    window: durationText(atQuarter, t),
  });
  const last = useRef(0);
  useEffect(() => {
    if (quarter <= last.current) return;
    last.current = quarter;
    setSpoken((s) => [...s, valueText].slice(-4));
  }, [quarter, valueText]);
  return (
    <div className="flex flex-col gap-1.5" data-slot="update-health-wait">
      <Progress
        value={Math.min(100, Math.round((Math.max(0, elapsed) / windowSeconds) * 100))}
        aria-label={t("updates.console.healthWait.label")}
        aria-valuetext={valueText}
      />
      <p aria-live="off" className="text-sm text-muted-foreground">
        {t("updates.console.healthWait.text", { name, window: durationText(left, t) })}
      </p>
      <div aria-live="polite" className="sr-only">
        {spoken.map((message, i) => (
          // biome-ignore lint/suspicious/noArrayIndexKey: a message is only ever added at the end
          <p key={i}>{message}</p>
        ))}
      </div>
    </div>
  );
}

// UpdateProgress is the one bar of the console: how many steps have ended well, with the count beside
// it. Its movement is not announced.
function UpdateProgress({ items }: { items: readonly StackJobItem[] }) {
  const t = useT();
  const done = items.filter((i) => i.state === "done").length;
  const value = items.length === 0 ? 0 : Math.round((done / items.length) * 100);
  return (
    <div className="flex flex-col gap-1.5">
      <Progress value={value} aria-label={t("updates.console.progress.label")} />
      <p aria-live="off" className="text-sm text-muted-foreground">
        {t("updates.console.progress.text", { done, count: items.length })}
      </p>
    </div>
  );
}

// Announcer is the polite region of the console. It speaks the outcome once, as the job ends in front
// of the person, says nothing about a job as it was when the console opened, and never moves focus.
function Announcer({ job, outcome }: { job: UpdateJobView; outcome: UpdateOutcome }) {
  const t = useT();
  const before = useRef<string | null>(null);
  const [spoken, setSpoken] = useState<string>("");
  useEffect(() => {
    const was = before.current;
    before.current = job.state;
    if (was !== "running" || job.state === "running") return;
    const key = {
      done: "updated",
      cancelled: "cancelled",
      rolled_back: "rolledBack",
      rollback_failed: "rollbackFailed",
      pull_failed: "pullFailed",
      failed: "failed",
    }[outcome];
    setSpoken(t(`updates.console.announce.${key}`, { name: job.workload }));
  }, [job.state, job.workload, outcome, t]);
  return (
    <div data-slot="update-announcer" aria-live="polite" className="sr-only">
      {spoken ? <p>{spoken}</p> : null}
    </div>
  );
}

// OpenService goes to the services (or containers) of the environment, filtered to the one updated.
function OpenService({ envId, job }: { envId: string; job: UpdateJobView }) {
  const t = useT();
  const name = job.workload;
  return (
    <Button asChild variant="outline" size="sm" className="max-md:h-11">
      {job.kind === "container" ? (
        <Link to="/env/$envId/containers" params={{ envId }} search={{ q: name }}>
          {t("updates.console.openService", { name })}
        </Link>
      ) : (
        <Link to="/env/$envId/services" params={{ envId }} search={{ q: name }}>
          {t("updates.console.openService", { name })}
        </Link>
      )}
    </Button>
  );
}

// UpdateAnyway is "Update anyway…": a person's choice to try again a version whose update failed (D-14).
// It opens the confirm in its Update anyway form, which shows why it failed before and is the only thing
// that sends force. It needs the permissions of any update: services.control, and containers.control as
// well for a container. Below them it is shown unavailable with the reason, as its neighbours are.
function UpdateAnyway({ envId, job }: { envId: string; job: UpdateJobView }) {
  const t = useT();
  const envName = useEnvName(envId);
  const button = useRef<HTMLButtonElement>(null);
  const [open, setOpen] = useState(false);
  const service = useNeeds("services.control", envId);
  const container = useNeeds("containers.control", envId);
  // A stack is updated anyway through its own page, and a bulk update has no one workload to try again.
  if (job.kind === "stack" || job.kind === "bulk") return null;
  const needs = job.kind === "container" && service.allowed ? container : service;
  const reason = needs.reason
    ? t("updates.menu.needs", { needs: needs.reason, environment: envName })
    : undefined;
  return (
    <>
      <NeedsTooltip reason={needs.allowed ? undefined : reason}>
        <Button
          ref={button}
          type="button"
          variant="outline"
          size="sm"
          className="max-md:h-11"
          disabled={!needs.allowed && !reason}
          onClick={() => setOpen(true)}
        >
          {t("updates.console.updateAnyway")}
        </Button>
      </NeedsTooltip>
      {open ? (
        <UpdateConfirmDialog
          envId={envId}
          envName={envName}
          workload={{ kind: job.kind, name: job.workload }}
          onClose={() => setOpen(false)}
          onClosed={() => button.current?.focus()}
        />
      ) : null}
    </>
  );
}

// The actions under a result that is not a success: where the workload is now, and a way to try again.
function FailureActions({ envId, job }: { envId: string; job: UpdateJobView }) {
  return (
    <div className="flex flex-wrap items-center gap-2">
      <OpenService envId={envId} job={job} />
      <UpdateAnyway envId={envId} job={job} />
    </div>
  );
}

// Result is how the update ended: the success Alert, or the failure with what TierPlan did about it. A
// running update has none. A reason is always TierPlan's own words for the code of the step, never the
// engine's message; the one text taken from the server is the step's own sentence for a failure that
// has no code of its own.
function Result({
  envId,
  job,
  outcome,
}: {
  envId: string;
  job: UpdateJobView;
  outcome: UpdateOutcome;
}) {
  const t = useT();
  if (job.state === "running") return null;
  const name = job.workload;
  const old = job.fromDigest ? shortDigest(job.fromDigest) : "";
  if (outcome === "done") {
    return (
      <Alert role="status" data-slot="update-result">
        <CircleCheck aria-hidden="true" className="text-status-online-text" />
        <AlertTitle>{t("updates.console.success.title")}</AlertTitle>
        <AlertDescription className="flex flex-col items-start gap-2 break-words">
          <span className="break-all">
            {job.toDigest
              ? t("updates.console.success.body", { name, digest: shortDigest(job.toDigest) })
              : t("updates.console.success.bodyNoDigest", { name })}
          </span>
          <OpenService envId={envId} job={job} />
        </AlertDescription>
      </Alert>
    );
  }
  if (outcome === "cancelled") {
    return (
      <Alert variant="warning" role="status" data-slot="update-result">
        <AlertTitle>{t("updates.console.cancelled.title")}</AlertTitle>
        <AlertDescription className="flex flex-col items-start gap-2">
          <span className="break-all">{t("updates.console.cancelled.body", { name })}</span>
          <OpenService envId={envId} job={job} />
        </AlertDescription>
      </Alert>
    );
  }
  if (outcome === "pull_failed") {
    // The image could not be pulled: nothing was changed, so there was nothing to roll back, and the
    // words never say there was.
    return (
      <Alert variant="warning" role="status" data-slot="update-result">
        <TriangleAlert aria-hidden="true" />
        <AlertTitle>{t("updates.console.pullFailed.title")}</AlertTitle>
        <AlertDescription className="flex flex-col items-start gap-2">
          <span className="break-all">
            {old
              ? t("updates.console.pullFailed.body", { name, old })
              : t("updates.console.pullFailed.bodyNoDigest", { name })}
          </span>
          <FailureActions envId={envId} job={job} />
        </AlertDescription>
      </Alert>
    );
  }
  if (outcome === "rolled_back") {
    const reason = rollbackReasonText(t, causeOf(job), {
      subject: name,
      windowSeconds: windowOf(job),
    });
    return (
      <Alert variant="warning" role="status" data-slot="update-result">
        <Undo2 aria-hidden="true" />
        <AlertTitle>{t("updates.console.rolledBack.title")}</AlertTitle>
        <AlertDescription className="flex flex-col items-start gap-2">
          <span className="break-all">
            {old
              ? t("updates.console.rolledBack.body", { reason, old })
              : t("updates.console.rolledBack.bodyNoDigest", { reason })}
          </span>
          <FailureActions envId={envId} job={job} />
        </AlertDescription>
      </Alert>
    );
  }
  if (outcome === "rollback_failed") {
    const restore = (job.items ?? []).find((i) => isRestoreStep(i) && i.state === "failed");
    const reason = restoreReasonText(t, restore?.code ?? "restore_failed");
    return (
      <Alert variant="destructive" role="alert" data-slot="update-result">
        <CircleX aria-hidden="true" />
        <AlertTitle>{t("updates.console.rollbackFailed.title")}</AlertTitle>
        <AlertDescription className="flex flex-col items-start gap-2">
          <span className="break-all">
            {old
              ? t("updates.console.rollbackFailed.body", { name, old, reason })
              : t("updates.console.rollbackFailed.bodyNoDigest", { name, reason })}
          </span>
          <FailureActions envId={envId} job={job} />
        </AlertDescription>
      </Alert>
    );
  }
  // Another failure. When someone else changed the service while the update failed, the step that says so
  // is the restore's; otherwise it is the step that stopped.
  const restoreChanged = (job.items ?? []).find(
    (i) => i.id === "rollback" && i.code === "restore_service_changed",
  );
  const reason = (restoreChanged ?? failedStepOf(job))?.message ?? "";
  return (
    <Alert variant="destructive" role="alert" data-slot="update-result">
      <CircleX aria-hidden="true" />
      <AlertTitle>{t("updates.console.failed.title")}</AlertTitle>
      <AlertDescription className="flex flex-col items-start gap-2">
        <span className="break-all">
          {reason
            ? t("updates.console.failed.body", { reason, name })
            : t("updates.console.failed.bodyNoReason", { name })}
        </span>
        <OpenService envId={envId} job={job} />
      </AlertDescription>
    </Alert>
  );
}

// UpdateConsole is the live view of an image update (IMG-06, D-05): the P7 deploy console's parts for
// a plain service. It is a view of a job on the server, so leaving never stops the update: opening it
// again shows the job as it stands and follows it from there, and a finished one shows how it ended
// with no live chip. The title takes the focus when the route opens, once. Every name, label and
// message is text.
export function UpdateConsole({ envId, jobId }: UpdateConsoleProps) {
  const t = useT();
  const envName = useEnvName(envId);
  const follow = useImageUpdateJob(envId, jobId);
  const now = useNow();
  const title = useRef<HTMLHeadingElement>(null);
  const job = follow.job;

  // Focus moves to the title once, when the route opens and the title is there.
  const titled = !!job;
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
            {t("updates.console.gone.title")}
          </EmptyTitle>
          <EmptyDescription>{t("updates.console.gone.body")}</EmptyDescription>
        </EmptyHeader>
        <EmptyContent>
          <Button asChild variant="outline" className="max-md:h-11">
            <Link to="/env/$envId/services" params={{ envId }}>
              {t("updates.console.gone.open")}
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
          <span>{t("updates.console.error.title")}</span>
          <span>{t("updates.console.error.body")}</span>
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="max-md:h-11"
            onClick={follow.reload}
          >
            {t("updates.console.error.retry")}
          </Button>
        </AlertDescription>
      </Alert>
    );
  }

  if (!job) {
    return (
      <div role="status" aria-label={t("updates.console.loading")} className="flex flex-col gap-3">
        <Skeleton className="h-7 w-64" />
        <Skeleton className="h-6 w-full max-w-md" />
        <Skeleton className="h-6 w-full max-w-md" />
      </div>
    );
  }

  const ended = job.state !== "running";
  const outcome = outcomeOf(job, follow.output);
  const chip =
    !ended && !follow.finished && (follow.status === "live" || follow.status === "reconnecting")
      ? follow.status
      : undefined;
  const startedAt = formatRelative(job.createdAt, new Date(now), t);
  const steps = consoleStepsOf(t, job);
  const row = rowOf(job);
  // A container with a health check is waited for after it starts: the time it may still take shows.
  const healthItem =
    job.kind === "container"
      ? (job.items ?? []).find((i) => i.id === "health" && i.state === "running")
      : undefined;

  return (
    <div className="mx-auto flex w-full max-w-[960px] flex-col gap-6">
      <nav aria-label={t("updates.console.breadcrumb.label")}>
        <ol className="flex flex-wrap items-center gap-x-2 text-xs text-muted-foreground">
          <li className="min-w-0 break-all">
            {envName}
            <span aria-hidden="true"> /</span>
          </li>
          <li className="min-w-0 break-all" aria-current="page">
            {t("updates.console.breadcrumb.update", { name: job.workload })}
          </li>
        </ol>
      </nav>
      <div className="flex flex-col gap-1">
        <div className="flex flex-wrap items-center justify-between gap-3">
          <h1
            ref={title}
            tabIndex={-1}
            className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xl font-semibold outline-none"
          >
            <span className="break-all">{t("updates.console.title", { name: job.workload })}</span>
            {ended ? (
              <span className="inline-flex items-center gap-1.5 text-base font-medium">
                {outcome === "done" ? (
                  <CircleCheck aria-hidden="true" className="size-4 text-status-online-text" />
                ) : outcome === "rolled_back" ? (
                  <Undo2 aria-hidden="true" className="size-4 text-status-warning-text" />
                ) : outcome === "pull_failed" ? (
                  <TriangleAlert aria-hidden="true" className="size-4 text-status-warning-text" />
                ) : (
                  <CircleX aria-hidden="true" className="size-4 text-destructive" />
                )}
                {t(`updates.console.outcome.${outcome}`)}
              </span>
            ) : null}
          </h1>
          {chip ? <LiveChip state={chip} /> : null}
        </div>
        <p className="text-sm text-muted-foreground">
          {job.startedBy
            ? t("updates.console.started", { time: startedAt, user: job.startedBy })
            : t("updates.console.startedAnon", { time: startedAt })}{" "}
          <Elapsed job={job} />
        </p>
      </div>
      <UpdateProgress items={steps} />
      {healthItem && !ended ? (
        <HealthWait name={job.workload} windowSeconds={healthWindowOf(healthItem)} />
      ) : null}
      <DeployStepList items={steps} envId={envId} />
      <ul aria-label={t("stackDeploy.steps.services")} className="flex flex-col">
        <DeployServiceRow item={row} />
      </ul>
      <DeployOutput lines={follow.output} failed={job.state === "failed"} />
      <Announcer job={job} outcome={outcome} />
      <div className="flex flex-col gap-3">
        <Result envId={envId} job={job} outcome={outcome} />
      </div>
    </div>
  );
}
