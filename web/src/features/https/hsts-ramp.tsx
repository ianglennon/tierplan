// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Check, Circle, Minus } from "lucide-react";
import { useId } from "react";

import type { HstsStep, HstsStepState, HstsView } from "@/features/https/card-state";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

const STEP_ICON: Record<HstsStepState, { Icon: typeof Check; className: string }> = {
  done: { Icon: Check, className: "text-status-online-text" },
  current: { Icon: Circle, className: "fill-current" },
  later: { Icon: Circle, className: "text-muted-foreground" },
  unused: { Icon: Minus, className: "text-muted-foreground" },
};

// HstsRamp is the lifetime steps (D-11, D-22, UI-SPEC J7): four steps as an ordered list, the step being
// sent named in words, so the ramp reads without colour and without a chart. The steps and their states
// come from hstsView; this only draws them. Shown while the setting is on, in every state.
export function HstsRamp({ view }: { view: HstsView }) {
  const t = useT();
  const headingId = useId();
  if (view.steps.length === 0) return null;
  return (
    <section className="flex min-w-0 flex-col gap-2">
      <h4 id={headingId} className="text-sm font-semibold">
        {t("https.hsts.steps.title")}
      </h4>
      <ol aria-labelledby={headingId} className="flex flex-col gap-2">
        {view.steps.map((step) => (
          <RampItem key={step.n} step={step} />
        ))}
      </ol>
      {view.startsLater ? (
        <p className="text-xs text-muted-foreground">{t("https.hsts.steps.startsLater")}</p>
      ) : null}
      <p className="text-xs text-muted-foreground">{t("https.hsts.steps.foot")}</p>
    </section>
  );
}

function RampItem({ step }: { step: HstsStep }) {
  const t = useT();
  const { Icon, className } = STEP_ICON[step.state];
  const current = step.state === "current";
  return (
    <li
      data-state={step.state}
      className={cn(
        "flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1 text-sm",
        step.state === "later" || step.state === "unused" ? "text-muted-foreground" : undefined,
      )}
    >
      <Icon className={cn("size-4 shrink-0", className)} aria-hidden="true" />
      <span className={current ? "font-medium" : undefined}>
        {t(`https.hsts.steps.label.${step.n}`)}
      </span>
      <code className="break-all font-mono text-xs">
        {t("https.hsts.lifetimeValue", { seconds: step.maxAge })}
      </code>
      {current ? <span className="text-xs">{t("https.hsts.steps.current")}</span> : null}
      {step.state === "unused" ? (
        <span className="text-xs">{t("https.hsts.steps.unused")}</span>
      ) : null}
      <span className="sr-only">
        {`(${t(step.state === "unused" ? "https.hsts.steps.unusedState" : `https.hsts.steps.${step.state}`)})`}
      </span>
    </li>
  );
}
