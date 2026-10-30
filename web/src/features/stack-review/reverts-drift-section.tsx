// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { ArrowRight, TriangleAlert } from "lucide-react";
import { useId, useState } from "react";

import type { StackReview } from "@/api/stacks";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { settingLabel } from "@/features/spec-editor/spec-diff";
import { DriftCause } from "@/features/stack-drift/drift-cause";
import { useT } from "@/i18n/use-t";

type Revert = NonNullable<StackReview["revertsDrift"]>[number];

// The list collapses past this many rows (UI-SPEC "Review additions", overflow).
const COLLAPSE_AFTER = 5;

// What the server writes for a value it hides; it is shown as the bullets the rest of the review uses.
const HIDDEN = "<hidden>";

interface RevertsDriftSectionProps {
  // The outside changes the deploy would put back, as the review's answer lists them.
  reverts: Revert[];
  // The drift could not be computed: the section is replaced by one line and the rest of the review stays.
  unavailable?: boolean;
  // What is reviewed, which words the title: a deploy, an update of images or a roll back.
  mode?: "deploy" | "update" | "rollback";
}

type Translate = (key: string, options?: Record<string, unknown>) => string;

// Value is a setting's value drawn as text: mono, wrapped anywhere, a hidden one as bullets and a missing one as a
// muted "not set". It is never markup.
function Value({ text, t }: { text: string; t: Translate }) {
  if (text === "") {
    return <span className="text-xs text-muted-foreground">{t("stackRevertsDrift.unset")}</span>;
  }
  return <span className="font-mono text-xs break-all">{text.replaceAll(HIDDEN, "••••")}</span>;
}

// RevertRow is one change the deploy would put back, with the cause under it (D-24): the service, the setting in
// words with its path in mono under it, what runs now and what the deploy sets, or for a whole service that it is
// created again or removed.
function RevertRow({ revert, t }: { revert: Revert; t: Translate }) {
  const service = (
    <span data-slot="service-name" className="font-mono text-sm font-medium break-all">
      {revert.service}
    </span>
  );
  if (revert.change !== "changed") {
    return (
      <li className="flex flex-col gap-0.5 py-2 first:pt-0 last:pb-0">
        <div className="flex flex-wrap items-baseline gap-x-2 text-sm">
          {service}
          <span>{t(`stackRevertsDrift.${revert.change}`)}</span>
        </div>
        <div className="text-xs text-muted-foreground">
          <DriftCause cause={revert.cause} />
        </div>
      </li>
    );
  }
  return (
    <li className="flex flex-col gap-0.5 py-2 first:pt-0 last:pb-0">
      <div className="flex flex-wrap items-baseline gap-x-2 gap-y-0.5 text-sm">
        {service}
        <span>{settingLabel(t, revert.path)}</span>
        <span className="flex flex-wrap items-baseline gap-x-1.5">
          <Value text={revert.live} t={t} />
          <ArrowRight
            aria-hidden="true"
            className="size-3.5 translate-y-0.5 text-muted-foreground"
          />
          <span className="sr-only">{t("stackRevertsDrift.arrow")}</span>
          <Value text={revert.desired} t={t} />
        </span>
      </div>
      <div className="font-mono text-xs break-all text-muted-foreground">{revert.path}</div>
      <div className="text-xs text-muted-foreground">
        <DriftCause cause={revert.cause} />
      </div>
    </li>
  );
}

// RevertsDriftSection is the warning above the Summary of a deploy, an update or a roll back review (UI-SPEC "Review
// additions", D-25): the changes made outside the stack that this deploy would put back, apart from the draft's own, so
// nobody undoes a hot-fix by accident. It informs and never blocks, so it is a headed region and not an alert, and
// Deploy stack does not look at it. It draws nothing when nothing would be reverted; when the server could not compute
// the drift it is one line saying so. Every value is text.
export function RevertsDriftSection({
  reverts,
  unavailable = false,
  mode = "deploy",
}: RevertsDriftSectionProps) {
  const t = useT();
  const titleId = useId();
  const listId = useId();
  const [all, setAll] = useState(false);
  if (unavailable) {
    return (
      <Alert role="status" data-slot="reverts-drift-unavailable">
        <TriangleAlert aria-hidden="true" />
        <AlertDescription>{t("stackRevertsDrift.unavailable")}</AlertDescription>
      </Alert>
    );
  }
  if (reverts.length === 0) return null;
  const rollback = mode === "rollback";
  const shown = all ? reverts : reverts.slice(0, COLLAPSE_AFTER);
  const context = mode === "deploy" ? {} : { context: mode };
  return (
    <section aria-labelledby={titleId} data-slot="reverts-drift">
      <Alert variant="warning" role="presentation">
        <TriangleAlert aria-hidden="true" />
        <AlertTitle id={titleId} className="break-words">
          <h4>{t("stackRevertsDrift.title", { count: reverts.length, ...context })}</h4>
        </AlertTitle>
        <AlertDescription className="flex flex-col gap-3">
          <p className="mb-0!">
            {t(rollback ? "stackRevertsDrift.body_rollback" : "stackRevertsDrift.body")}
          </p>
          <ul
            id={listId}
            aria-label={t(rollback ? "stackRevertsDrift.list_rollback" : "stackRevertsDrift.list")}
            className="flex flex-col divide-y"
          >
            {shown.map((r) => (
              <RevertRow key={`${r.service}:${r.change}:${r.path}`} revert={r} t={t} />
            ))}
          </ul>
          {reverts.length > COLLAPSE_AFTER ? (
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="self-start max-md:h-11"
              aria-expanded={all}
              aria-controls={listId}
              onClick={() => setAll(!all)}
            >
              {all
                ? t("stackRevertsDrift.showFewer")
                : t("stackRevertsDrift.showAll", { count: reverts.length })}
            </Button>
          ) : null}
        </AlertDescription>
      </Alert>
    </section>
  );
}
