// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { Check, CircleX, Minus } from "lucide-react";
import { useEffect, useRef, useState } from "react";

import {
  isRolloutRunning,
  type ServiceActionError,
  serviceQuery,
  serviceRolloutQuery,
} from "@/api/services";
import { AgentUpdateNotice } from "@/components/agent-update-notice";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Spinner } from "@/components/ui/spinner";
import { useEnvName } from "@/features/resources/use-env-name";
import { itemOf } from "@/features/service-detail/header-actions";
import { editRefusalOf } from "@/features/service-editor/edit-refusal";
import type { StagedKind } from "@/features/service-editor/staged";
import { ForceUpdateDialog } from "@/features/services/force-update-dialog";
import { RollbackDialog } from "@/features/services/rollback-dialog";
import { RolloutProgress } from "@/features/services/rollout-progress";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";

// How long, after an update was accepted, the page waits for the swarm to start it before it
// reads the update once more. An object so a test can shorten it.
export const applyTiming = { settleMs: 2000 };

type StepState = "waiting" | "working" | "done" | "failed";

interface ApplyProgressProps {
  envId: string;
  // The service being updated, or, for a create, the one that was made (empty until it is).
  serviceId: string;
  // Whether the service is being made or changed. A new service has no rolling update to follow.
  mode?: "edit" | "create";
  // The service's name. Text from Docker, drawn as text.
  name: string;
  // Where the apply request is: out, accepted, or refused.
  status: "pending" | "success" | "error";
  error?: ServiceActionError;
  // Whether the image is looked up before the update: it is when the image changed.
  lookup: boolean;
  // The objects the apply creates before the service, in the order it creates them.
  objects?: readonly { kind: StagedKind; name: string }[];
  // Back to the editor, with the edits kept.
  onBack: () => void;
  // Leave the editor for the service.
  onClose: () => void;
  // Load the service again and start over from what it is now.
  onReload: () => void;
}

function StepMark({ state }: { state: StepState }) {
  switch (state) {
    case "working":
      return <Spinner aria-hidden="true" />;
    case "done":
      return <Check aria-hidden="true" className="size-4 text-status-online-text" />;
    case "failed":
      return <CircleX aria-hidden="true" className="size-4 text-destructive" />;
    default:
      return <Minus aria-hidden="true" className="size-4 text-muted-foreground" />;
  }
}

function Step({ label, state }: { label: string; state: StepState }) {
  const t = useT();
  return (
    <li className="flex items-center gap-2 text-sm">
      <StepMark state={state} />
      <span className="min-w-0 break-words">{label}</span>
      <span className="text-muted-foreground">{t(`specEditor.apply.status.${state}`)}</span>
    </li>
  );
}

// ApplyProgress is the third state of the editor (D-03): the steps of the update in order (the
// image, each object made first, the service), each with its state as a word beside its mark, and
// then the real rolling update. The server does all of it in one request, so a pending request shows
// the first step working and the rest waiting, and a refusal says which step it stopped at and what
// became of the objects it had made: removed again, or named when they could not be. Nothing is
// retried (P2 D-42): a refusal is worded where it happened, with the way back to the editor and the
// way out of it.
export function ApplyProgress({
  envId,
  serviceId,
  mode = "edit",
  name,
  status,
  error,
  lookup,
  objects = [],
  onBack,
  onClose,
  onReload,
}: ApplyProgressProps) {
  const t = useT();
  const environment = useEnvName(envId);
  const create = mode === "create";
  const needs = useNeeds("services.control", envId);
  const detail = useQuery({ ...serviceQuery(envId, serviceId), enabled: !create });
  const accepted = status === "success";
  const rollout = useQuery({
    ...serviceRolloutQuery(envId, serviceId),
    enabled: accepted && !create,
  });
  const [dialog, setDialog] = useState<"rollback" | "retry" | null>(null);
  const [settled, setSettled] = useState(false);
  const sawRunning = useRef(false);

  const state = rollout.data?.state;
  const running = isRolloutRunning(state);
  const refetch = rollout.refetch;
  useEffect(() => {
    if (!accepted || settled || state === undefined) return;
    if (running) {
      sawRunning.current = true;
      return;
    }
    if (sawRunning.current) {
      setSettled(true);
      return;
    }
    // The swarm may not have started the update when the first answer was read, and a change to
    // an update policy alone starts none. Read it once more before calling it finished.
    const timer = setTimeout(async () => {
      const again = await refetch();
      if (!isRolloutRunning(again.data?.state)) setSettled(true);
    }, applyTiming.settleMs);
    return () => clearTimeout(timer);
  }, [accepted, settled, state, running, refetch]);

  const refusal =
    status === "error"
      ? editRefusalOf(t, error, {
          environment,
          image: "",
          name,
          action: mode === "create" ? "create" : "update",
        })
      : undefined;
  // The steps in order, each with where the request has got to: the first is working while it is
  // out; once refused, those before the one that failed are done and those after it wait.
  const sequence: { key: string; label: string }[] = [
    ...(lookup ? [{ key: "lookup", label: t("specEditor.apply.steps.lookup") }] : []),
    ...objects.map((o) => ({
      key: `${o.kind}:${o.name}`,
      label: t(`specEditor.apply.steps.${o.kind}`, { name: o.name }),
    })),
    {
      key: "update",
      label: create
        ? t("specEditor.create.apply.step", { name })
        : t("specEditor.apply.steps.update", { name }),
    },
  ];
  const failedAt = (): number => {
    if (!refusal) return -1;
    if (refusal.step === "lookup") return sequence.findIndex((s) => s.key === "lookup");
    const first = sequence.findIndex((s) => s.key !== "lookup");
    if (refusal.denied) return first;
    if (refusal.failedObject) return sequence.findIndex((s) => s.key === refusal.failedObject);
    return sequence.length - 1;
  };
  const failed = failedAt();
  const stateOf = (index: number, key: string): StepState => {
    if (status === "pending") return index === 0 ? "working" : "waiting";
    if (status === "success") return "done";
    if (key === "lookup")
      return index < failed || failed < 0 ? "done" : index === failed ? "failed" : "waiting";
    if (index === failed) return "failed";
    if (index > failed) return "waiting";
    // Before the one that failed: the lookup was done, and an object only if it was made.
    if (key === "update") return "done";
    const nth = sequence.slice(0, index).filter((s) => s.key !== "lookup").length;
    return nth < (refusal?.made ?? 0) ? "done" : "waiting";
  };
  const complete = settled && (state === "completed" || state === "none");
  const running_ = detail.data?.running ?? 0;

  return (
    <section className="flex flex-col gap-6">
      <h2 className="text-base font-semibold break-words">
        {create
          ? status === "success"
            ? t("specEditor.create.apply.done", { name })
            : t("specEditor.create.apply.title", { name })
          : t("specEditor.apply.title", { name })}
      </h2>
      <ol aria-label={t("specEditor.apply.steps.label")} className="flex flex-col gap-2">
        {sequence.map((step, i) => (
          <Step key={step.key} label={step.label} state={stateOf(i, step.key)} />
        ))}
      </ol>
      {refusal ? (
        <>
          {refusal.agentPolicy ? (
            <AgentUpdateNotice envId={envId} />
          ) : (
            <Alert variant="destructive">
              <CircleX aria-hidden="true" />
              <AlertDescription className="break-words">{refusal.message}</AlertDescription>
            </Alert>
          )}
          <div className="flex flex-wrap gap-2">
            {refusal.reload ? (
              <Button type="button" className="max-md:h-11" onClick={onReload}>
                {t("specEditor.apply.reload")}
              </Button>
            ) : null}
            <Button type="button" variant="outline" className="max-md:h-11" onClick={onBack}>
              {t("specEditor.apply.back")}
            </Button>
            <Button type="button" variant="outline" className="max-md:h-11" onClick={onClose}>
              {t("specEditor.apply.close")}
            </Button>
          </div>
        </>
      ) : null}
      {accepted && create ? (
        <div>
          <Button asChild className="max-md:h-11">
            <Link to="/env/$envId/services/$serviceId" params={{ envId, serviceId }}>
              {t("specEditor.apply.view")}
            </Link>
          </Button>
        </div>
      ) : null}
      {accepted && !create ? (
        <>
          <RolloutProgress
            envId={envId}
            serviceId={serviceId}
            name={name}
            onRollback={() => setDialog("rollback")}
            onRetry={() => setDialog("retry")}
          />
          {complete ? (
            <div className="flex flex-col gap-1">
              <h3 className="text-base font-semibold">{t("specEditor.apply.done.title")}</h3>
              <p className="text-sm text-muted-foreground">
                {t("specEditor.apply.done.body", { name, count: running_ })}
              </p>
            </div>
          ) : null}
          <div>
            <Button asChild className="max-md:h-11">
              <Link to="/env/$envId/services/$serviceId" params={{ envId, serviceId }}>
                {t("specEditor.apply.view")}
              </Link>
            </Button>
          </div>
          {needs.allowed && detail.data ? (
            <>
              <ForceUpdateDialog
                envId={envId}
                target={itemOf(detail.data)}
                retry
                open={dialog === "retry"}
                onOpenChange={(open) => setDialog(open ? "retry" : null)}
              />
              <RollbackDialog
                envId={envId}
                target={itemOf(detail.data)}
                open={dialog === "rollback"}
                onOpenChange={(open) => setDialog(open ? "rollback" : null)}
              />
            </>
          ) : null}
        </>
      ) : null}
    </section>
  );
}
