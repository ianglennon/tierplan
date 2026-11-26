// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { type UseMutationResult, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";

import type { HealthState } from "@/api/environments";
import { IngressActionError, ingressKey } from "@/api/ingress";
import {
  type IngressJob,
  type IngressStarted,
  ingressJobQuery,
  jobEnded,
  type ReviewPreview,
} from "@/api/ingress-switches";
import type { components } from "@/api/schema";
import { StatusDot } from "@/components/status-dot";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Spinner } from "@/components/ui/spinner";
import { useT } from "@/i18n/use-t";

type StackJobItemBody = components["schemas"]["StackJobItemBody"];

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

// How many stacks the redeploy list names before it says "and n more" (UI-SPEC S6).
export const REDEPLOY_SHOWN = 10;

// The switches the dialog is built for. Each has its own words under ingressPage.review.<kind>.
export type ReviewKind = "enable" | "disable";

export interface ReviewDialogProps {
  envId: string;
  kind: ReviewKind;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  // Told when a job this dialog started begins and stops running, so the card behind it can say so.
  onRunning?: (running: boolean) => void;
  // The switch's preview, run when the dialog opens. It writes nothing.
  preview: UseMutationResult<ReviewPreview, Error, void>;
  // The switch itself: it answers with the job to follow.
  start: UseMutationResult<IngressStarted, Error, void>;
}

// ReviewDialog is the house pattern for every ingress switch (D-07, UI-SPEC S6): one dialog with three states in place,
// never three dialogs. First the server's preview, read when the dialog opens, with the findings that refuse the switch
// or warn about it; the confirm waits for the preview and is off while a finding refuses. Then the job the confirm
// started, read every 2 s until it ends (no stream, D-24), step by step. Then how it ended. The dialog cannot be closed
// while the job runs, and closing it refreshes the page.
export function ReviewDialog({
  envId,
  kind,
  open,
  onOpenChange,
  onRunning,
  preview,
  start,
}: ReviewDialogProps) {
  const queryClient = useQueryClient();
  // A job that is running holds the dialog open: it has one button and that button waits for the end.
  const running = useRef(false);

  function change(next: boolean) {
    if (!next && running.current) return;
    if (!next) {
      preview.reset();
      start.reset();
      void queryClient.invalidateQueries({ queryKey: ingressKey(envId) });
    }
    onOpenChange(next);
  }

  return (
    <Dialog open={open} onOpenChange={change}>
      <DialogContent aria-describedby={undefined} showCloseButton={false}>
        {open ? (
          <ReviewBody
            envId={envId}
            kind={kind}
            preview={preview}
            start={start}
            onRunning={(value) => {
              running.current = value;
              onRunning?.(value);
            }}
            onClose={() => change(false)}
          />
        ) : null}
      </DialogContent>
    </Dialog>
  );
}

// Refusal is a write the server refused, in the server's own words and in the dialog: operation_locked names who holds
// the environment. A failure with no detail (the network, a proxy) gets the common sentence.
function Refusal({ error }: { error: unknown }) {
  const t = useT();
  const detail = error instanceof IngressActionError ? error.detail : undefined;
  return (
    <Alert variant="destructive" role="alert">
      <AlertDescription className="break-words">
        {detail ?? t("common.mutationFailed")}
      </AlertDescription>
    </Alert>
  );
}

interface ReviewBodyProps {
  envId: string;
  kind: ReviewKind;
  preview: UseMutationResult<ReviewPreview, Error, void>;
  start: UseMutationResult<IngressStarted, Error, void>;
  onRunning: (running: boolean) => void;
  onClose: () => void;
}

function ReviewBody({ envId, kind, preview, start, onRunning, onClose }: ReviewBodyProps) {
  const t = useT();
  const queryClient = useQueryClient();
  const [jobId, setJobId] = useState<string | null>(null);
  // Set when the dialog mounts, so the preview is asked for once however often the dialog renders.
  const asked = useRef(false);
  // Set the moment the confirm is pressed, ahead of the render that disables it, so a double press starts one job.
  const sending = useRef(false);
  const run = preview.mutate;

  useEffect(() => {
    if (asked.current) return;
    asked.current = true;
    run();
  }, [run]);

  const job = useQuery({ ...ingressJobQuery(envId, jobId ?? ""), enabled: jobId !== null });
  const ended = jobId !== null && (jobEnded(job.data) || (job.isError && !job.isFetching));
  const holding = jobId !== null && !ended;
  useEffect(() => {
    onRunning(holding || start.isPending);
  }, [holding, start.isPending, onRunning]);

  // The page behind the dialog shows the new state the moment the job ends, not only when the dialog closes.
  const finished = jobEnded(job.data);
  useEffect(() => {
    if (finished) void queryClient.invalidateQueries({ queryKey: ingressKey(envId) });
  }, [finished, envId, queryClient]);

  function confirm() {
    if (sending.current || start.isPending) return;
    sending.current = true;
    start.mutate(undefined, {
      onSettled: () => {
        sending.current = false;
      },
      onSuccess: (started) => setJobId(started.jobId),
    });
  }

  if (jobId !== null) {
    return (
      <Progress
        kind={kind}
        job={job.data}
        unreadable={job.isError}
        ended={ended}
        onClose={onClose}
      />
    );
  }

  const data = preview.data;
  const findings = data?.findings ?? [];
  const blocking = findings.filter((f) => f.severity === "error");
  const warnings = findings.filter((f) => f.severity === "warning");
  const redeploy = data?.redeploy ?? [];
  const unreachable = data?.unreachable ?? [];
  const checking = !preview.isError && !data;
  const confirmOff = checking || preview.isError || blocking.length > 0 || start.isPending;

  return (
    <div className="flex flex-col gap-4">
      <DialogHeader>
        <DialogTitle className="break-words">{t(`ingressPage.review.${kind}.title`)}</DialogTitle>
      </DialogHeader>
      <p className="text-sm">{t(`ingressPage.review.${kind}.body`)}</p>
      {checking ? (
        <p role="status" className="flex items-center gap-2 text-sm text-muted-foreground">
          <Spinner aria-hidden="true" />
          {t("ingressPage.review.checking")}
        </p>
      ) : null}
      {preview.isError ? <Refusal error={preview.error} /> : null}
      {blocking.length > 0 ? (
        <Alert variant="destructive">
          <AlertTitle>{t("ingressPage.review.blocked")}</AlertTitle>
          <AlertDescription>
            <Findings findings={blocking} />
          </AlertDescription>
        </Alert>
      ) : null}
      {warnings.length > 0 ? (
        <Alert variant="warning">
          <AlertDescription>
            <Findings findings={warnings} />
          </AlertDescription>
        </Alert>
      ) : null}
      {unreachable.length > 0 ? (
        <Alert>
          <AlertDescription>
            {t("ingressPage.review.unreachable", { nodes: unreachable.join(", ") })}
          </AlertDescription>
        </Alert>
      ) : null}
      {redeploy.length > 0 ? (
        <div className="flex flex-col gap-2">
          <h4 className="text-sm font-semibold">{t("ingressPage.review.redeploy.title")}</h4>
          <ul className="flex flex-col gap-1">
            {redeploy.slice(0, REDEPLOY_SHOWN).map((stack) => (
              <li key={stack.stackId} className="font-mono text-xs break-all">
                {stack.stackName}
              </li>
            ))}
          </ul>
          {redeploy.length > REDEPLOY_SHOWN ? (
            <p className="text-xs text-muted-foreground">
              {t("ingressPage.review.redeploy.more", { n: redeploy.length - REDEPLOY_SHOWN })}
            </p>
          ) : null}
          <p className="text-xs text-muted-foreground">{t("ingressPage.review.redeploy.note")}</p>
        </div>
      ) : null}
      {start.isError ? <Refusal error={start.error} /> : null}
      <DialogFooter>
        <Button
          type="button"
          variant="outline"
          className={TOUCH_HEIGHT}
          disabled={start.isPending}
          onClick={onClose}
        >
          {t(`ingressPage.review.${kind}.dismiss`)}
        </Button>
        <Button
          type="button"
          variant={kind === "disable" ? "destructive" : "default"}
          className={TOUCH_HEIGHT}
          disabled={confirmOff}
          onClick={confirm}
        >
          {start.isPending ? <Spinner aria-hidden="true" /> : null}
          {t(`ingressPage.review.${kind}.confirm`)}
        </Button>
      </DialogFooter>
    </div>
  );
}

// Findings draws the server's sentences as sent, each with the things it names (a port, a stack, a hostname) in mono.
function Findings({ findings }: { findings: ReviewPreview["findings"] }) {
  return (
    <ul className="flex flex-col gap-2">
      {findings.map((finding, i) => (
        // A finding has a stable code but the same code may repeat, so the place in the list completes the key.
        // biome-ignore lint/suspicious/noArrayIndexKey: findings are static for as long as the preview shows.
        <li key={`${finding.code}-${i}`} className="flex flex-col gap-1">
          <span className="break-words">{finding.message}</span>
          {(finding.items ?? []).length > 0 ? (
            <ul className="flex flex-col gap-0.5">
              {(finding.items ?? []).map((item) => (
                <li key={item} className="font-mono text-xs break-all">
                  {item}
                </li>
              ))}
            </ul>
          ) : null}
        </li>
      ))}
    </ul>
  );
}

const STEP_DOT: Record<StackJobItemBody["state"], HealthState> = {
  done: "online",
  running: "reconnecting",
  failed: "offline",
  cancelled: "offline",
  waiting: "pending",
  skipped: "pending",
};

// failureSentence is the job's own words for why it stopped: the sentence of the first step that failed, else its
// detail, else the common one.
function failureSentence(job: IngressJob | undefined, fallback: string): string {
  const failed = (job?.items ?? []).find((item) => item.state === "failed");
  return failed?.message || failed?.detail || fallback;
}

interface ProgressProps {
  kind: ReviewKind;
  job: IngressJob | undefined;
  unreadable: boolean;
  ended: boolean;
  onClose: () => void;
}

// Progress is the dialog once the switch is confirmed: the step list and the job's output while it runs, the outcome
// when it ended, and one button that waits for the end.
function Progress({ kind, job, unreadable, ended, onClose }: ProgressProps) {
  const t = useT();
  const steps = (job?.items ?? []).filter((item) => item.kind !== "group" && item.kind !== "task");
  const output = job?.output ?? [];
  const succeeded = job?.state === "done";
  const failed = ended && job !== undefined && !succeeded;
  return (
    <div className="flex flex-col gap-4">
      <DialogHeader>
        <DialogTitle className="break-words">
          {t(`ingressPage.review.${kind}.progress`)}
        </DialogTitle>
      </DialogHeader>
      {steps.length > 0 ? (
        <ul className="flex flex-col gap-2">
          {steps.map((step) => (
            <li key={step.id} className="flex items-center gap-2 text-sm">
              <StatusDot state={STEP_DOT[step.state]} label={step.label} />
              <span className="sr-only">{t(`ingressPage.review.stepState.${step.state}`)}</span>
            </li>
          ))}
        </ul>
      ) : ended ? null : (
        <p className="flex items-center gap-2 text-sm text-muted-foreground">
          <Spinner aria-hidden="true" />
          {t(`ingressPage.review.${kind}.progress`)}
        </p>
      )}
      <div
        role="log"
        aria-live="polite"
        aria-label={t("ingressPage.review.log")}
        className="max-h-48 overflow-auto rounded-md bg-muted p-2 font-mono text-xs break-words whitespace-pre-wrap"
      >
        {output.map((line) => (
          <div key={line.seq}>{line.text}</div>
        ))}
      </div>
      {succeeded ? (
        <Alert>
          <AlertDescription>{t(`ingressPage.review.${kind}.done`)}</AlertDescription>
        </Alert>
      ) : null}
      {failed ? (
        <Alert variant="destructive">
          <AlertDescription className="flex flex-col gap-2 break-words">
            <span>{failureSentence(job, t("ingressPage.review.jobFailed"))}</span>
            <span className="text-sm">{t(`ingressPage.review.${kind}.failedHint`)}</span>
          </AlertDescription>
        </Alert>
      ) : null}
      {unreadable && job === undefined ? (
        <Alert variant="destructive">
          <AlertDescription>{t("ingressPage.review.jobUnreadable")}</AlertDescription>
        </Alert>
      ) : null}
      <DialogFooter>
        <Button type="button" className={TOUCH_HEIGHT} disabled={!ended} onClick={onClose}>
          {failed ? t("ingressPage.review.close") : t("ingressPage.review.closeDetails")}
        </Button>
      </DialogFooter>
    </div>
  );
}
