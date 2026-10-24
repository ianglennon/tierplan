// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { HealthState } from "@/api/environments";
import type { StackRevision } from "@/api/stack-history";
import { StatusDot } from "@/components/status-dot";
import { TruncatedText } from "@/features/resources/resource-table";
import { keyExists, useT } from "@/i18n/use-t";

// The status colour of each outcome (UI-SPEC "Color"): good is online, failed is offline, and a revision
// whose deploy is still running is the pulsing warning dot, which StatusDot turns off under reduced motion.
const TONE: Record<StackRevision["outcome"], HealthState> = {
  good: "online",
  failed: "offline",
  pending: "reconnecting",
};

type Translate = (key: string, options?: Record<string, unknown>) => string;

// failureReason is the sentence for why a revision failed, from its code. It is the wording of the
// failed-deploy alert (stackDeploy.lastFailed.reason) when that has words for the code, the server's own
// sentence otherwise, and the plain one when neither does, so a code is never shown as a code.
export function failureReason(t: Translate, item: StackRevision): string {
  const key = `stackDeploy.lastFailed.reason.${item.code}`;
  if (item.code && keyExists(key)) return t(key);
  if (item.code === "interrupted") return t("stackHistory.result.interrupted");
  return item.reason || t("stackHistory.result.unfinished");
}

// failureText is the reason with the service the deploy stopped at in front of it, when it names one.
export function failureText(t: Translate, item: StackRevision): string {
  const reason = failureReason(t, item);
  return item.failedService
    ? t("stackHistory.detail.failed.body", { service: item.failedService, reason })
    : reason;
}

interface RevisionResultCellProps {
  item: StackRevision;
}

// RevisionResultCell is the result of a revision as a dot and a word, never colour alone. A failed one
// adds the reason in muted text under the word, clamped to one line with the whole text in a Tooltip. The
// reason is Docker's and Bayplan's text, drawn as text.
export function RevisionResultCell({ item }: RevisionResultCellProps) {
  const t = useT();
  return (
    <span className="flex min-w-0 flex-col">
      <StatusDot state={TONE[item.outcome]} label={t(`stackHistory.result.${item.outcome}`)} />
      {item.outcome === "failed" ? (
        <span className="text-muted-foreground">
          <TruncatedText className="text-xs">{failureText(t, item)}</TruncatedText>
        </span>
      ) : null}
    </span>
  );
}
