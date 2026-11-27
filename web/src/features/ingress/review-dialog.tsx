// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { type UseMutationResult, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { type ReactNode, useEffect, useRef, useState } from "react";

import type { HealthState } from "@/api/environments";
import { IngressActionError, IngressLoadError, ingressKey } from "@/api/ingress";
import {
  type IngressJob,
  type IngressStarted,
  ingressJobQuery,
  jobEnded,
  type ReviewItem,
  type ReviewPreview,
} from "@/api/ingress-switches";
import type { components } from "@/api/schema";
import { CopyableCode } from "@/components/copyable-code";
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
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { POLICY } from "@/features/ingress/vip-status-card";
import { keyExists, useT } from "@/i18n/use-t";

type StackJobItemBody = components["schemas"]["StackJobItemBody"];

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

// How many stacks the redeploy list names before it says "and n more" (UI-SPEC S6).
export const REDEPLOY_SHOWN = 10;

// The switches the dialog is built for. Each has its own words under ingressPage.review.<kind>.
export type ReviewKind =
  | "enable"
  | "disable"
  | "vipEnable"
  | "vipChange"
  | "vipDisable"
  | "selfEnable"
  | "selfDisable";

export type { ReviewItem };

// ReviewForm is the optional first state of the dialog (the virtual IP's address and router ID): fields the card owns,
// drawn above the buttons, and the button that runs the preview with what was typed. The preview is not run when the
// dialog opens, since there is nothing to check yet. A refusal of the preview comes back to this state, above the
// fields, so the person can correct the form and check again.
export interface ReviewForm {
  fields: ReactNode;
  // Whether what was typed is enough to ask the server about.
  canCheck: boolean;
  check: () => void;
}

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
  // A form to fill before the preview, for a switch that needs input.
  form?: ReviewForm;
  // Values for the placeholders of the dialog's body.
  bodyValues?: Record<string, unknown>;
  // A warning the card knows and the server cannot, drawn above the preview's list: the page is being used through the
  // route the switch takes away (UI-SPEC S1c, D-11). It never disables the confirm.
  caution?: string;
  // A second sentence for the Done alert, read from the job that ended (UI-SPEC S1b: the self route is on but TierPlan's
  // service is not on the tierplan-self network yet). Undefined or an empty answer adds nothing.
  doneNote?: (job: IngressJob) => string | undefined;
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
  form,
  bodyValues,
  caution,
  doneNote,
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
            form={form}
            bodyValues={bodyValues}
            caution={caution}
            doneNote={doneNote}
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
  form?: ReviewForm;
  bodyValues?: Record<string, unknown>;
  caution?: string;
  doneNote?: (job: IngressJob) => string | undefined;
  onRunning: (running: boolean) => void;
  onClose: () => void;
}

function ReviewBody({
  envId,
  kind,
  preview,
  start,
  form,
  bodyValues,
  caution,
  doneNote,
  onRunning,
  onClose,
}: ReviewBodyProps) {
  const t = useT();
  const queryClient = useQueryClient();
  const [jobId, setJobId] = useState<string | null>(null);
  // Set when the dialog mounts, so the preview is asked for once however often the dialog renders.
  const asked = useRef(false);
  // Set the moment the confirm is pressed, ahead of the render that disables it, so a double press starts one job.
  const sending = useRef(false);
  const run = preview.mutate;

  const hasForm = form !== undefined;
  useEffect(() => {
    // With a form there is nothing to ask until it has been filled in: its own button runs the preview.
    if (asked.current || hasForm) return;
    asked.current = true;
    run();
  }, [run, hasForm]);

  const job = useQuery({ ...ingressJobQuery(envId, jobId ?? ""), enabled: jobId !== null });
  // Only a job the server will not show any more ends the wait for it (403, 404, which stop the polling too). Any other failed
  // read is one that missed: the job runs on and the next poll tries again, so the dialog stays held and is not closable.
  const unreadable =
    job.error instanceof IngressLoadError && (job.error.status === 403 || job.error.status === 404);
  const ended = jobId !== null && (jobEnded(job.data) || unreadable);
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
        unreadable={unreadable}
        ended={ended}
        bodyValues={bodyValues}
        doneNote={doneNote}
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
  const checking = hasForm ? preview.isPending : !preview.isError && !data;
  // An ordered preview (the self route's) refuses through an item the server marked refused, not through a finding.
  const items = data?.items ?? [];
  const blockedByItem = items.some((item) => item.state === "refused");
  const confirmOff =
    checking || preview.isError || blocking.length > 0 || blockedByItem || start.isPending;
  const checkLabel = keyExists(`ingressPage.review.${kind}.check`)
    ? t(`ingressPage.review.${kind}.check`)
    : t("ingressPage.review.check");

  // Until a preview with nothing that refuses has come back, a switch with a form shows the form, and what the server
  // refused with is drawn above it, as the server's sentence and never as a toast (U-17).
  if (form && (!data || blocking.length > 0 || blockedByItem)) {
    return (
      <div className="flex flex-col gap-4">
        <DialogHeader>
          <DialogTitle className="break-words">{t(`ingressPage.review.${kind}.title`)}</DialogTitle>
        </DialogHeader>
        <p className="text-sm">{t(`ingressPage.review.${kind}.body`, bodyValues)}</p>
        {preview.isError ? <Refusal error={preview.error} /> : null}
        {blocking.length > 0 ? (
          <Alert variant="destructive">
            <AlertTitle>{t("ingressPage.review.blocked")}</AlertTitle>
            <AlertDescription>
              <Findings findings={blocking} />
            </AlertDescription>
          </Alert>
        ) : null}
        {blockedByItem ? <BlockedItems items={items} bodyValues={bodyValues} /> : null}
        {form.fields}
        {checking ? (
          <p role="status" className="flex items-center gap-2 text-sm text-muted-foreground">
            <Spinner aria-hidden="true" />
            {t("ingressPage.review.checking")}
          </p>
        ) : null}
        <DialogFooter>
          <Button type="button" variant="outline" className={TOUCH_HEIGHT} onClick={onClose}>
            {t(`ingressPage.review.${kind}.dismiss`)}
          </Button>
          <Button
            type="button"
            className={TOUCH_HEIGHT}
            disabled={!form.canCheck || preview.isPending}
            onClick={form.check}
          >
            {preview.isPending ? <Spinner aria-hidden="true" /> : null}
            {checkLabel}
          </Button>
        </DialogFooter>
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-4">
      <DialogHeader>
        <DialogTitle className="break-words">{t(`ingressPage.review.${kind}.title`)}</DialogTitle>
      </DialogHeader>
      <p className="text-sm">{t(`ingressPage.review.${kind}.body`, bodyValues)}</p>
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
      {blockedByItem ? <BlockedAlert /> : null}
      {caution ? (
        <Alert variant="warning">
          <AlertDescription className="break-words">{caution}</AlertDescription>
        </Alert>
      ) : null}
      {items.length > 0 ? <ReviewItems items={items} bodyValues={bodyValues} /> : null}
      {data?.warning !== undefined ? (
        // The virtual IP is never switched on without this in front of the person, so a preview that came without the
        // sentence still shows the one the page knows.
        <Alert variant="warning">
          <AlertTitle>{t("ingressPage.review.vip.warningTitle")}</AlertTitle>
          <AlertDescription className="break-words">
            {data.warning !== "" ? data.warning : t("ingressPage.review.vip.warningText")}
          </AlertDescription>
        </Alert>
      ) : null}
      {(data?.managers ?? []).length > 0 ? (
        <PreviewManagers managers={data?.managers ?? []} />
      ) : null}
      {warnings.length > 0 ? (
        <Alert variant="warning">
          <AlertDescription>
            <Findings findings={warnings} />
          </AlertDescription>
        </Alert>
      ) : null}
      {data?.notice ? (
        <Alert>
          <AlertDescription className="break-words">{data.notice}</AlertDescription>
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

// BlockedAlert is the one destructive alert above an ordered preview that has a refused item. It says that nothing was
// changed and points at the item; the refused sentence itself is drawn once, in the list.
function BlockedAlert() {
  const t = useT();
  return (
    <Alert variant="destructive">
      <AlertTitle>{t("ingressPage.review.blocked")}</AlertTitle>
      <AlertDescription className="break-words">
        {t("ingressPage.review.blockedBody")}
      </AlertDescription>
    </Alert>
  );
}

// BlockedItems is the preview as the form state draws it when an item was refused: the alert, then the list with the
// refused item in it, above the fields the person corrects.
function BlockedItems({
  items,
  bodyValues,
}: {
  items: ReviewItem[];
  bodyValues?: Record<string, unknown>;
}) {
  return (
    <>
      <BlockedAlert />
      <ReviewItems items={items} bodyValues={bodyValues} />
    </>
  );
}

// ITEM_DOT is how an ordered preview's item is drawn: a passed one is online, a warning is degraded, a refusal is
// offline. The word beside it is for the reader who cannot see the colour.
const ITEM_DOT: Record<ReviewItem["state"], HealthState> = {
  ok: "online",
  warning: "degraded",
  refused: "offline",
};

// ReviewItems draws an ordered preview (D-17, UI-SPEC A-05): one li per item, in the order the server sends. The order is
// the story, so nothing is regrouped by severity. The web supplies each heading from the item's code; the message is the
// server's sentence as sent, drawn as text.
function ReviewItems({
  items,
  bodyValues,
}: {
  items: ReviewItem[];
  bodyValues?: Record<string, unknown>;
}) {
  const t = useT();
  const direct = typeof bodyValues?.direct === "string" ? bodyValues.direct : undefined;
  return (
    <ol className="flex flex-col gap-4">
      {items.map((item) => {
        const headingKey = `ingressPage.review.items.${item.code}`;
        return (
          // The server sends one item per code, so the code is the key.
          <li key={item.code} className="flex flex-col gap-1">
            <StatusDot
              state={ITEM_DOT[item.state]}
              label={keyExists(headingKey) ? t(headingKey) : item.code}
              className="text-sm font-medium"
            />
            <span className="sr-only">{t(`ingressPage.review.itemState.${item.state}`)}</span>
            <p className="text-sm break-words">{item.message}</p>
            {item.snippet ? (
              <div className="flex flex-col gap-2">
                <p className="text-sm font-medium">{t("ingressPage.review.snippet.title")}</p>
                <CopyableCode
                  code={item.snippet.text}
                  label={t(`ingressPage.review.snippet.${item.snippet.kind}`)}
                />
                {item.snippet.kind === "stack_file" ? (
                  <p className="text-xs break-words text-muted-foreground">
                    {t("ingressPage.review.snippet.stackFileNote")}
                  </p>
                ) : null}
                {direct !== undefined ? (
                  <p className="text-xs break-words text-muted-foreground">
                    {t("ingressPage.review.snippet.note", { direct })}
                  </p>
                ) : null}
              </div>
            ) : null}
          </li>
        );
      })}
    </ol>
  );
}

// PreviewManagers is the managers table of the virtual IP's preview: each manager and whether its agent lets services
// run privileged, the thing the address needs on every one of them.
function PreviewManagers({ managers }: { managers: NonNullable<ReviewPreview["managers"]> }) {
  const t = useT();
  return (
    <div className="overflow-x-auto">
      <Table aria-label={t("ingressPage.nodes.vip.managers.label")}>
        <TableHeader>
          <TableRow>
            <TableHead scope="col">{t("ingressPage.nodes.vip.managers.manager")}</TableHead>
            <TableHead scope="col">{t("ingressPage.nodes.vip.managers.privileged")}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {managers.map((m) => {
            const policy = POLICY[m.state];
            return (
              <TableRow key={m.nodeId}>
                <TableCell className="align-top text-sm break-all whitespace-normal">
                  {m.hostname !== "" ? m.hostname : m.nodeId}
                  {m.note ? (
                    <span className="block text-xs text-muted-foreground">{m.note}</span>
                  ) : null}
                </TableCell>
                <TableCell className="align-top whitespace-normal">
                  <StatusDot state={policy.state} label={t(policy.key)} />
                </TableCell>
              </TableRow>
            );
          })}
        </TableBody>
      </Table>
    </div>
  );
}

// HTTPS_FIX are the findings fixed on Settings > HTTPS (UI-SPEC S4): TierPlan's own certificate uses the HTTP-01
// challenge, which needs the port 80 the ingress takes, so the fix is to switch it to DNS-01 there.
// ingress_http01_conflict: TierPlan runs here and is not on the tierplan-self network.
// ingress_http01_unknown (a warning): TierPlan couldn't tell whether that conflict applies.
const HTTPS_FIX: ReadonlySet<string> = new Set([
  "ingress_http01_conflict",
  "ingress_http01_unknown",
]);

// Findings draws the server's sentences as sent, each with the things it names (a port, a stack, a hostname) in mono.
function Findings({ findings }: { findings: ReviewPreview["findings"] }) {
  const t = useT();
  return (
    <ul className="flex flex-col gap-2">
      {findings.map((finding, i) => (
        // A finding has a stable code but the same code may repeat, so the place in the list completes the key.
        // biome-ignore lint/suspicious/noArrayIndexKey: findings are static for as long as the preview shows.
        <li key={`${finding.code}-${i}`} className="flex flex-col gap-1">
          <span className="break-words">{finding.message}</span>
          {HTTPS_FIX.has(finding.code) ? (
            <Link
              to="/settings/https"
              className="self-start rounded-sm text-accent-ink hover:underline"
            >
              {t("ingressPage.review.openHttps")}
            </Link>
          ) : null}
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
  // The values of the placeholders in the failure hint (the direct address of the self route).
  bodyValues?: Record<string, unknown>;
  doneNote?: (job: IngressJob) => string | undefined;
  onClose: () => void;
}

// Progress is the dialog once the switch is confirmed: the step list and the job's output while it runs, the outcome
// when it ended, and one button that waits for the end.
function Progress({ kind, job, unreadable, ended, bodyValues, doneNote, onClose }: ProgressProps) {
  const t = useT();
  const steps = (job?.items ?? []).filter((item) => item.kind !== "group" && item.kind !== "task");
  const output = job?.output ?? [];
  const succeeded = job?.state === "done";
  const failed = ended && job !== undefined && !succeeded;
  const note = succeeded && job ? doneNote?.(job) : undefined;
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
          <AlertDescription className="flex flex-col gap-1 break-words">
            <span>{t(`ingressPage.review.${kind}.done`)}</span>
            {note ? <span>{note}</span> : null}
          </AlertDescription>
        </Alert>
      ) : null}
      {failed ? (
        <Alert variant="destructive">
          <AlertDescription className="flex flex-col gap-2 break-words">
            <span>{failureSentence(job, t("ingressPage.review.jobFailed"))}</span>
            <span className="text-sm">
              {t(`ingressPage.review.${kind}.failedHint`, bodyValues)}
            </span>
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
