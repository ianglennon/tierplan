// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Check, Circle, CircleX, Minus, TriangleAlert } from "lucide-react";

import type { ReactNode } from "react";

import { RelativeTime } from "@/components/relative-time";
import { Spinner } from "@/components/ui/spinner";
import { formatDate } from "@/features/https/copy";
import {
  type ACMERun,
  type ACMERunStreamState,
  type ACMEStep,
  type ACMEStepState,
  stepsFor,
} from "@/features/https/https-run-stream";
import { rich, Sentence, SLOT } from "@/features/https/sentence";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

interface RunPanelProps {
  stream: ACMERunStreamState;
  // What the order is for, as text: the names, and the host Let's Encrypt checks. They fill the hints.
  names: string;
  host: string;
  // When the certificate now served stops being valid; given once the status shows the new one.
  validUntil?: string;
}

// The header of a run: what it is doing while it runs, how it ended after. A renewal the scheduler
// started says so (UI-SPEC "Run panel").
function headerKey(run: ACMERun): string {
  const renew = run.kind === "renew";
  switch (run.state) {
    case "running":
      if (!renew) return "https.run.issuing";
      return run.trigger === "schedule" ? "https.run.renewingAuto" : "https.run.renewing";
    case "done":
      return renew ? "https.run.renewed" : "https.run.issued";
    case "failed":
      return renew ? "https.run.renewFailed" : "https.run.issueFailed";
    case "cancelled":
      return renew ? "https.run.renewStopped" : "https.run.issueStopped";
  }
}

// What a row shows: the step's own state, or "leftOver" for a Clean-up that is done but could not delete
// every record it wrote (UI-SPEC "Run panel - DNS-01 steps"). The run itself is unchanged by it.
type RowState = ACMEStepState | "leftOver";

// A Clean-up step the server ended done with the class left_over.
const isLeftOver = (step: ACMEStep | undefined): boolean =>
  step?.id === "cleanup" && step.state === "done" && step.code === "left_over";

function StepIcon({ state }: { state: RowState }) {
  switch (state) {
    case "leftOver":
      return (
        <TriangleAlert aria-hidden="true" className="size-4 shrink-0 text-status-warning-text" />
      );
    case "running":
      return <Spinner aria-hidden="true" role="presentation" className="size-4 shrink-0" />;
    case "done":
      return <Check aria-hidden="true" className="size-4 shrink-0 text-status-online-text" />;
    case "failed":
      return <CircleX aria-hidden="true" className="size-4 shrink-0 text-destructive" />;
    case "skipped":
    case "cancelled":
      return <Minus aria-hidden="true" className="size-4 shrink-0 text-muted-foreground" />;
    default:
      return <Circle aria-hidden="true" className="size-4 shrink-0 text-muted-foreground" />;
  }
}

// stepHint is the sentence under a step: what it is doing while it runs, and for Clean-up what it could
// not do once it is done. The values of a DNS-01 step (the record, the name it was delegated to, the
// resolvers) are the server's and are drawn as mono text that breaks anywhere.
function stepHint(
  t: ReturnType<typeof useT>,
  id: string,
  step: ACMEStep | undefined,
  state: RowState,
  dns: boolean,
  values: { names: string; host: string },
): ReactNode {
  if (state === "leftOver") {
    return t("https.run.hint.cleanupLeftOver", { count: step?.leftOver ?? 1 });
  }
  if (state === "skipped" && id === "account") return t("https.run.hint.accountSkipped");
  if (state !== "running") return "";
  if (dns) {
    const record = step?.record ?? "";
    switch (id) {
      case "challenge":
        return step?.target
          ? rich(t, "https.run.hint.challengeDns01Delegated", {
              record,
              host: step.host ?? values.host,
              target: step.target,
            })
          : rich(t, "https.run.hint.challengeDns01", { record });
      case "propagation":
        return rich(t, "https.run.hint.propagation", {
          record,
          resolvers: step?.resolvers?.length
            ? step.resolvers.join(", ")
            : t("https.error.resolversDefault"),
        });
      case "cleanup":
        return t("https.run.hint.cleanup");
    }
  }
  return t(`https.run.hint.${id}`, values);
}

// RunPanel is the live view of a certificate run (D-09): the six steps of an HTTP-01 run, or the eight
// of a DNS-01 run (chosen from the run's challenge), with where each stands, fed by the run's stream. It is drawn for a run that is going and, once finished, stays until the next run
// starts. Step labels are the SPA's own words; nothing from the CA reaches it. Only the header is
// announced; the step list is not, so a screen reader is not read six updates a second.
export function RunPanel({ stream, names, host, validUntil }: RunPanelProps) {
  const t = useT();
  const run = stream.run;
  if (!run) return null;

  const byId = new Map<string, ACMEStep>((run.items ?? []).map((s) => [s.id, s]));
  const running = run.state === "running";
  const reconnecting = running && stream.status === "reconnecting";
  const dns = run.challenge === "dns-01";

  return (
    <div className="flex flex-col gap-3 rounded-lg border p-4" data-run-state={run.state}>
      <div className="flex flex-col gap-1">
        <p
          aria-live="polite"
          className="flex flex-wrap items-baseline gap-x-2 text-sm font-semibold"
        >
          <span>{t(headerKey(run))}</span>
          {reconnecting ? (
            <span className="text-xs font-normal text-muted-foreground">
              {t("https.run.reconnecting")}
            </span>
          ) : null}
        </p>
        {run.state === "done" && validUntil ? (
          <p className="text-xs text-muted-foreground">
            {t("https.run.validUntil", { date: formatDate(validUntil) })}
          </p>
        ) : null}
        <p className="text-xs text-muted-foreground">
          <Sentence
            text={t("https.run.started", { when: SLOT })}
            slot={<RelativeTime value={run.startedAt} />}
          />
        </p>
      </div>
      <ol aria-live="off" aria-label={t("https.run.steps")} className="flex flex-col gap-2">
        {stepsFor(run.challenge).map((id) => {
          const step = byId.get(id);
          const stepState: ACMEStepState = step?.state ?? "waiting";
          const state: RowState = isLeftOver(step) ? "leftOver" : stepState;
          const hint = stepHint(t, id, step, state, dns, { names, host });
          return (
            <li
              key={id}
              data-step={id}
              data-state={stepState}
              data-left-over={state === "leftOver" ? "true" : undefined}
              className={cn(
                "flex items-start gap-2 text-sm",
                state === "waiting" && "text-muted-foreground",
              )}
            >
              <span className="mt-0.5">
                <StepIcon state={state} />
              </span>
              <span className="flex min-w-0 flex-col">
                <span className="font-medium">
                  {t(`https.run.step.${id}`)}
                  <span className="sr-only"> ({t(`https.run.state.${state}`)})</span>
                </span>
                {hint ? (
                  <span className="min-w-0 break-all text-xs text-muted-foreground">{hint}</span>
                ) : null}
              </span>
            </li>
          );
        })}
      </ol>
    </div>
  );
}
