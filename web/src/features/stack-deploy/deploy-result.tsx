// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { Link, useNavigate } from "@tanstack/react-router";
import { CircleX, Info } from "lucide-react";
import { useRef } from "react";

import {
  type DeployRequestError,
  type StackJob,
  type StackJobItem,
  stackQuery,
  useRollbackStack,
} from "@/api/stacks";
import { AgentUpdateNotice } from "@/components/agent-update-notice";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { useNow } from "@/components/relative-time";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Spinner } from "@/components/ui/spinner";
import { useEnvName } from "@/features/resources/use-env-name";
import { CONVERGE_TIMEOUT } from "@/features/stack-deploy/deploy-service-row";
import { isStandalone } from "@/features/stack-deploy/deploy-step-list";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { withBase } from "@/lib/base-path";
import { formatRelative } from "@/lib/format";
import { toastError, toastInfo } from "@/lib/toast";

const isService = (i: StackJobItem): boolean => i.kind === "service";

// servicesOf is the service rows of a job, in order.
export function servicesOf(job: StackJob): StackJobItem[] {
  return (job.items ?? []).filter(isService);
}

// runningServices is how many services the deploy took to the swarm, for the sentence under a
// finished job: a row each when the server listed them, else what the services step counted.
export function runningServices(job: StackJob): number {
  const rows = servicesOf(job);
  if (rows.length > 0) return rows.filter((r) => r.state === "done").length;
  const step = (job.items ?? []).find((it) => it.id === "services");
  return step ? Math.max(step.total, step.current) : 0;
}

// failureOf is the item a failed job stopped at: the first service or step that failed, else the
// first that did not finish.
export function failureOf(job: StackJob): StackJobItem | undefined {
  const items = job.items ?? [];
  const failed = items.filter((it) => it.state === "failed");
  return (
    failed.find(isService) ??
    failed[0] ??
    items.find((it) => it.state === "cancelled" && it.code !== "not_run")
  );
}

// updatedServices is how many services the swarm took before the deploy stopped: those it finished
// with, and those whose write it accepted but that did not settle (a timeout counts, a rejection does
// not).
export function updatedServices(job: StackJob): number {
  return servicesOf(job).filter((r) => r.state === "done" || Boolean(r.result?.action)).length;
}

// The cleanup a deploy that stopped did: the objects it created and removed again, and the ones it
// could not remove. An object a service uses stays and is not reported: it is not a failure.
export interface Cleanup {
  removed: number;
  // "network s1_front", "secret s1_tok_ab12": what stays on the swarm that Bayplan could not remove.
  stuck: string[];
}

export function cleanupOf(job: StackJob): Cleanup {
  const out: Cleanup = { removed: 0, stuck: [] };
  for (const it of job.items ?? []) {
    if (it.result?.cleaned === "true") out.removed += 1;
    else if (it.result?.left === "failed") {
      out.stuck.push(`${it.result.kind ?? ""} ${it.result.name ?? ""}`.trim());
    }
  }
  return out;
}

type Translate = (key: string, options?: Record<string, unknown>) => string;

// rollbackRefusal words a roll back the server refused. The server decides every refusal; this only
// says what it means in the page's words.
function rollbackRefusal(t: Translate, err: DeployRequestError, envName: string): string {
  switch (err.type) {
    case "operation_locked":
      return t("stackConverge.lock.refused", { env: envName });
    case "no_previous_version":
      return t("stackDeploy.rollback.errors.noPrevious");
    case "privileged_compose":
      return t("stackDeploy.rollback.errors.needsAdmin");
    case "compose_invalid":
    case "compose_unsupported":
    case "stack_invalid":
    case "pins_invalid":
      return t("stackDeploy.rollback.errors.invalid");
    case "too_many_deploys":
      return t("stackDeploy.rollback.errors.tooMany");
    case "target_changed":
    case "target_engine_missing":
      return t("stackDeploy.rollback.errors.target");
    case "stack_name_clash":
      return t("stackDeploy.rollback.errors.clash");
  }
  if (err.status === 0) return t("stackDeploy.rollback.errors.lost");
  return t("stackDeploy.rollback.errors.generic");
}

// RollbackAction is the one-click way back after a deploy that stopped (UI-SPEC "Deploy console", D-14):
// the accent button names what it does and, under it, when that version was deployed and by whom. One
// click starts the roll back as a new job and the console is replaced by that job's. There is no
// dialog: the button already says which version it restores. With no earlier version, or for a person
// who may not deploy, it is off and says why. After a roll back that failed it reads Retry roll back.
// Nothing here rolls back by itself, and a mutation is never retried.
export function RollbackAction({
  envId,
  stackId,
  retry,
  variant = "default",
  caption = true,
}: {
  envId: string;
  stackId: string;
  // After a roll back that failed, the button reads Retry roll back.
  retry: boolean;
  // The accent on the console, where it is the way forward; an outline beside the other actions of the
  // stack's page.
  variant?: "default" | "outline";
  // Whether to say, under the button, when the version it restores was deployed and by whom.
  caption?: boolean;
}) {
  const t = useT();
  const envName = useEnvName(envId);
  const navigate = useNavigate();
  const stack = useQuery(stackQuery(envId, stackId));
  const needs = useNeeds("stacks.deploy", envId);
  const rollback = useRollbackStack(envId, stackId);
  const now = useNow();
  const back = stack.data?.rollback;
  const available = Boolean(back?.available);
  // The permission is the first reason; a person who may not deploy is not told about versions.
  const reason = !needs.allowed
    ? needs.reason
    : available
      ? undefined
      : t("stackDeploy.rollback.noPrevious");
  const label = t(retry ? "stackDeploy.rollback.retry" : "stackDeploy.rollback.action");

  // A second click that lands before the page has drawn the first one's pending state sends nothing.
  const sending = useRef(false);
  function start() {
    if (sending.current) return;
    sending.current = true;
    rollback.mutate(undefined, {
      onSettled: () => {
        sending.current = false;
      },
      onSuccess: ({ jobId }) =>
        // Replace, so Back leaves the console and does not return to the one that failed.
        void navigate({
          to: "/env/$envId/stacks/$stackId/deploys/$jobId",
          params: { envId, stackId, jobId },
          replace: true,
        }),
      onError: (err) => {
        const message = rollbackRefusal(t, err, envName);
        if (err.type === "no_previous_version") toastInfo(message);
        else toastError(message);
      },
    });
  }

  const button = (
    <Button
      type="button"
      variant={variant}
      className="max-md:h-11"
      disabled={rollback.isPending}
      onClick={start}
    >
      {rollback.isPending ? <Spinner aria-hidden="true" /> : null}
      {label}
    </Button>
  );
  const when =
    back?.deployedAt && available ? formatRelative(back.deployedAt, new Date(now), t) : "";
  return (
    <div className="flex flex-col items-start gap-1">
      <NeedsTooltip reason={reason}>{button}</NeedsTooltip>
      {caption && when ? (
        <span className="text-xs text-muted-foreground">
          {back?.deployedBy
            ? t("stackDeploy.rollback.lastDeployed", { time: when, user: back.deployedBy })
            : t("stackDeploy.rollback.lastDeployedAnon", { time: when })}
        </span>
      ) : null}
    </div>
  );
}

interface DeployResultProps {
  job: StackJob;
  envId: string;
  stackId: string;
  name: string;
  envName: string;
}

// CleanupReport is the second part of a stopped deploy's account: how many of the objects it created
// were removed again, and which it could not remove and where the person removes them. A cancel
// words the first part itself and shows only the second.
function CleanupReport({ cleanup, stuckOnly = false }: { cleanup: Cleanup; stuckOnly?: boolean }) {
  const t = useT();
  return (
    <>
      {!stuckOnly && cleanup.removed > 0 ? (
        <p className="break-words">
          {t("stackDeploy.cleanup.removed", { count: cleanup.removed })}
        </p>
      ) : null}
      {cleanup.stuck.length > 0 ? (
        <p className="break-words">
          {t("stackDeploy.cleanup.left", { list: cleanup.stuck.join(", ") })}
        </p>
      ) : null}
    </>
  );
}

// DeployResult is what the console says when the job has ended: Deploy complete with what is
// running, Deploy failed (role alert) with where it stopped, or Deploy cancelled, and the ways on.
// It is drawn only once, when the job ends; the console announces the outcome through its own
// polite region. Every name and message is text.
export function DeployResult({ job, envId, stackId, name, envName }: DeployResultProps) {
  const t = useT();
  const stack = (
    <Button asChild className="max-md:h-11">
      <Link to="/env/$envId/stacks/$stackId" params={{ envId, stackId }}>
        {t("stackDeploy.openStack")}
      </Link>
    </Button>
  );
  const outline = (
    <Button asChild variant="outline" className="max-md:h-11">
      <Link to="/env/$envId/stacks/$stackId" params={{ envId, stackId }}>
        {t("stackDeploy.openStack")}
      </Link>
    </Button>
  );
  const rolledBack = job.mode === "rollback";
  if (job.state === "done") {
    return (
      <section className="flex flex-col gap-3">
        <h4 className="text-base font-semibold">
          {t(rolledBack ? "stackDeploy.rollback.complete.title" : "stackDeploy.complete.title")}
        </h4>
        <p className="text-sm break-words">
          {t("stackDeploy.complete.body", { name, env: envName, count: runningServices(job) })}
        </p>
        <div className="flex flex-wrap gap-2">
          {stack}
          {/* The stack's Logs tab is a page of its own; until it exists this is a plain address. */}
          <Button asChild variant="outline" className="max-md:h-11">
            <a
              href={withBase(
                `/env/${encodeURIComponent(envId)}/stacks/${encodeURIComponent(stackId)}/logs`,
              )}
            >
              {t("stackDeploy.openLogs")}
            </a>
          </Button>
        </div>
      </section>
    );
  }
  if (job.state === "failed" || job.state === "partial") {
    const failed = failureOf(job);
    const rows = servicesOf(job);
    // With a row for each service the sentence says how far it got; without, it is the server's
    // own sentence for the step.
    const standalone = isStandalone(job.items ?? []);
    // A host its owner made read-only is the whole story: nothing was asked of it.
    const readOnly = standalone && failed?.code === "host_read_only";
    const sentence = readOnly
      ? t("actions.readOnlyHost")
      : rows.length > 0
        ? t("stackDeploy.failed.stopped", {
            step: failed?.label ?? name,
            done: updatedServices(job),
            count: rows.length,
          })
        : failed?.message
          ? failed.message
          : t("stackDeploy.failed.fallback", { step: failed?.label ?? name });
    // A service that never settled has its own sentence: how long it was given, and what to do.
    const timedOut = rows.find((r) => r.code === CONVERGE_TIMEOUT);
    const cleanup = cleanupOf(job);
    return (
      <Alert variant="destructive">
        <CircleX aria-hidden="true" />
        <AlertTitle>
          {t(rolledBack ? "stackDeploy.rollback.failed.title" : "stackDeploy.failed.title")}
        </AlertTitle>
        <AlertDescription className="flex flex-col gap-3">
          {timedOut ? (
            <p className="break-words">
              {t("stackConverge.timeout.body", {
                service: timedOut.label,
                count: Number(timedOut.result?.minutes) || 0,
              })}
            </p>
          ) : null}
          <p className="break-words">{sentence}</p>
          <CleanupReport cleanup={cleanup} />
          {standalone && failed?.code === "agent_policy_denied" ? (
            <AgentUpdateNotice envId={envId} />
          ) : null}
          <div className="flex flex-wrap items-start gap-2">
            <RollbackAction envId={envId} stackId={stackId} retry={rolledBack} />
            <Button asChild variant="outline" className="max-md:h-11">
              <Link to="/env/$envId/stacks/$stackId/editor" params={{ envId, stackId }}>
                {t("stackDeploy.backToEditing")}
              </Link>
            </Button>
            {outline}
          </div>
        </AlertDescription>
      </Alert>
    );
  }
  if (job.state === "cancelled") {
    const stoppedAt = failureOf(job);
    const rows = servicesOf(job);
    const cleanup = cleanupOf(job);
    return (
      <Alert>
        <Info aria-hidden="true" />
        <AlertTitle>
          {t(rolledBack ? "stackDeploy.rollback.cancelled.title" : "stackDeploy.cancelled.title")}
        </AlertTitle>
        <AlertDescription className="flex flex-col gap-3">
          {rows.length > 0 ? (
            <p className="break-words">
              {t("stackDeploy.cancelled.stopped", {
                step: stoppedAt?.label ?? name,
                done: updatedServices(job),
                count: rows.length,
              })}
              {cleanup.removed > 0 ? ` ${t("stackDeploy.cleanup.removedCancelled")}` : ""}
            </p>
          ) : (
            <p>{t("stackDeploy.cancelled.body")}</p>
          )}
          <CleanupReport cleanup={cleanup} stuckOnly />
          <div className="flex flex-wrap items-start gap-2">
            <RollbackAction envId={envId} stackId={stackId} retry={rolledBack} />
            <Button asChild variant="outline" className="max-md:h-11">
              <Link to="/env/$envId/stacks/$stackId/editor" params={{ envId, stackId }}>
                {t("stackDeploy.backToEditing")}
              </Link>
            </Button>
            {outline}
          </div>
        </AlertDescription>
      </Alert>
    );
  }
  return null;
}
