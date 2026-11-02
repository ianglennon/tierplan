// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { Check, CircleX, Minus } from "lucide-react";
import type { Ref } from "react";

import { isJobGone, type StackJobItem, stackJobQuery } from "@/api/stacks";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Spinner } from "@/components/ui/spinner";
import { reasonOf } from "@/features/stacks/follow-operation";
import { useT } from "@/i18n/use-t";

interface RemovalProgressProps {
  envId: string;
  stackId: string;
  stackName: string;
  jobId: string;
  // True when the definition was kept: the stack is still there, so Close becomes Open stack.
  keepDefinition: boolean;
  // The heading takes focus when the panel replaces the form.
  headingRef: Ref<HTMLHeadingElement>;
  onClose: () => void;
}

type State = StackJobItem["state"];

function StepMark({ state }: { state: State }) {
  switch (state) {
    case "running":
      return <Spinner aria-hidden="true" />;
    case "done":
      return <Check aria-hidden="true" className="size-4 text-status-online-text" />;
    case "failed":
      return <CircleX aria-hidden="true" className="size-4 text-destructive" />;
    default:
      return <Minus aria-hidden="true" className="size-4 text-muted-foreground" />;
  }
}

// RemovalProgress is the second phase of the Remove dialog (UI-SPEC "Remove dialog and removal job"): the
// steps of the removal as the job reports them, in the same dialog, with the pull panel's rules. Movement is
// not announced (aria-live="off" on the list); the end is announced once, politely. Closing the dialog never
// stops the job: it runs on the server, and the toast comes from followStackOperation, not from this panel.
// The job is read every second while it runs; the names a step reports are Docker's and drawn as text.
export function RemovalProgress({
  envId,
  stackId,
  stackName,
  jobId,
  keepDefinition,
  headingRef,
  onClose,
}: RemovalProgressProps) {
  const t = useT();
  const query = useQuery(stackJobQuery(envId, stackId, jobId));
  const job = query.data;
  const gone = isJobGone(query.error);
  const items = job?.items ?? [];
  const finished = job !== undefined && job.state !== "running";
  const ok = job?.state === "done";

  function announcement() {
    if (!finished || !job) return "";
    if (ok)
      return t(keepDefinition ? "stackRemove.progress.doneKept" : "stackRemove.progress.done", {
        name: stackName,
      });
    const reason = reasonOf(job);
    return reason
      ? t("stackRemove.progress.failed", { name: stackName, reason })
      : t("stackRemove.progress.failedNoReason", { name: stackName });
  }

  return (
    <>
      <DialogHeader>
        <DialogTitle ref={headingRef} tabIndex={-1} className="break-all outline-none">
          {t("stackRemove.progress.title", { name: stackName })}
        </DialogTitle>
        <DialogDescription>{t("stackRemove.progress.description")}</DialogDescription>
      </DialogHeader>
      {gone ? (
        <Alert>
          <AlertDescription>{t("stackRemove.progress.gone")}</AlertDescription>
        </Alert>
      ) : (
        <ol
          aria-label={t("stackRemove.progress.stepsLabel")}
          aria-live="off"
          className="flex flex-col gap-2"
        >
          {items.map((item) => (
            <li key={item.id} data-state={item.state} className="flex flex-col gap-1">
              <div className="flex items-center gap-2 text-sm">
                <StepMark state={item.state} />
                <span className="min-w-0 break-all">{item.label}</span>
                {item.id === "wait" && item.state === "running" && Number(item.result?.left) > 0 ? (
                  <span className="text-xs text-muted-foreground">
                    {t("stackRemove.progress.left", { count: Number(item.result?.left) })}
                  </span>
                ) : null}
                <span className="ms-auto text-xs text-muted-foreground">
                  {t(`stackRemove.progress.state.${item.state}`)}
                </span>
              </div>
              {item.state === "failed" ? (
                <div className="ms-6 flex flex-col gap-1 text-sm text-status-offline-text">
                  {item.result?.name ? (
                    <p className="font-mono text-xs break-all">{item.result.name}</p>
                  ) : null}
                  {item.message ? <p className="break-words">{item.message}</p> : null}
                  {item.detail ? (
                    <p className="font-mono text-xs break-all">{item.detail}</p>
                  ) : null}
                </div>
              ) : null}
            </li>
          ))}
        </ol>
      )}
      {finished ? (
        <>
          {ok ? null : (
            <Alert variant="destructive" role="alert">
              <AlertDescription className="break-words">{announcement()}</AlertDescription>
            </Alert>
          )}
          {/* The end is said once, politely, whether or not it is also drawn above. */}
          <p role="status" aria-live="polite" className={ok ? "text-sm" : "sr-only"}>
            {ok ? announcement() : ""}
          </p>
        </>
      ) : null}
      <DialogFooter>
        {keepDefinition && ok ? (
          <Button asChild variant="outline" className="max-md:h-11" onClick={onClose}>
            <Link to="/env/$envId/stacks/$stackId" params={{ envId, stackId }}>
              {t("stackRemove.progress.openStack")}
            </Link>
          </Button>
        ) : (
          <Button type="button" variant="outline" className="max-md:h-11" onClick={onClose}>
            {t("stackRemove.progress.close")}
          </Button>
        )}
      </DialogFooter>
    </>
  );
}
