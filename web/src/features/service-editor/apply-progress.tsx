// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

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
  serviceId: string;
  // The service's name. Text from Docker, drawn as text.
  name: string;
  // Where the apply request is: out, accepted, or refused.
  status: "pending" | "success" | "error";
  error?: ServiceActionError;
  // Whether the image is looked up before the update: it is when the image changed.
  lookup: boolean;
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

// ApplyProgress is the third state of the editor (D-03): the steps of the update in order, each
// with its state as a word beside its mark, and then the real rolling update. The server does the
// lookup and the update in one request, so a pending request shows the lookup working and the
// update waiting, and a refusal says which of the two it stopped at. Nothing is retried (P2 D-42):
// a refusal is worded where it happened, with the way back to the editor and the way out of it.
export function ApplyProgress({
  envId,
  serviceId,
  name,
  status,
  error,
  lookup,
  onBack,
  onClose,
  onReload,
}: ApplyProgressProps) {
  const t = useT();
  const environment = useEnvName(envId);
  const needs = useNeeds("services.control", envId);
  const detail = useQuery(serviceQuery(envId, serviceId));
  const accepted = status === "success";
  const rollout = useQuery({ ...serviceRolloutQuery(envId, serviceId), enabled: accepted });
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
    status === "error" ? editRefusalOf(t, error, { environment, image: "" }) : undefined;
  const lookupState: StepState =
    status === "pending"
      ? "working"
      : status === "success"
        ? "done"
        : refusal?.step === "lookup"
          ? "failed"
          : "done";
  const updateState: StepState =
    status === "pending"
      ? lookup
        ? "waiting"
        : "working"
      : status === "success"
        ? "done"
        : refusal?.step === "lookup"
          ? "waiting"
          : "failed";
  const complete = settled && (state === "completed" || state === "none");
  const running_ = detail.data?.running ?? 0;

  return (
    <section className="flex flex-col gap-6">
      <h2 className="text-base font-semibold break-words">
        {t("specEditor.apply.title", { name })}
      </h2>
      <ol aria-label={t("specEditor.apply.steps.label")} className="flex flex-col gap-2">
        {lookup ? <Step label={t("specEditor.apply.steps.lookup")} state={lookupState} /> : null}
        <Step label={t("specEditor.apply.steps.update", { name })} state={updateState} />
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
      {accepted ? (
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
