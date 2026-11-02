// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Trans } from "react-i18next";

import type { StackDriftCause } from "@/api/stack-drift";
import { RelativeTime } from "@/components/relative-time";
import { useT } from "@/i18n/use-t";

interface DriftCauseProps {
  // The cause the server named, absent when the audit log knows none.
  cause?: StackDriftCause | null;
  // How many drifted services TierPlan's own actions explain. More than one reads as a count with the latest of them.
  count?: number;
  // The stack's name, for the sentence about an adoption.
  stackName?: string;
  // The short form of a Tooltip: no time, and nothing at all when the cause is not known.
  short?: boolean;
}

// The sentence for a cause that is not an adoption, as the i18n key of its action.
const FULL_KEYS = {
  scaled: "stackDriftCause.scaled",
  rolled_back: "stackDriftCause.rolled_back",
  edited: "stackDriftCause.edited",
} as const;

// useCauseText is the short sentence of a cause, as the badge's Tooltip reads it: who did what in TierPlan, with no time.
// An empty string says there is nothing to add.
export function useCauseText({ cause, count = 1, stackName }: DriftCauseProps): string {
  const t = useT();
  if (!cause) return "";
  if (cause.action === "adopted") {
    return stackName
      ? t("stackDriftCause.adopted", { name: stackName })
      : t("stackDriftCause.adoptedStack");
  }
  if (count > 1) return t("stackDriftCause.short.multiple", { count, user: cause.by });
  return t(`stackDriftCause.short.${cause.action}`, { service: cause.service, user: cause.by });
}

// DriftCause is the line that says why a stack differs from what was deployed (UI-SPEC "Cause line", D-24): a service
// that was scaled, rolled back or edited in TierPlan, with who and when; several such changes, with the latest; a
// difference that was there when the stack was adopted; or, when the audit log knows nothing, that it changed outside
// TierPlan. Every name in it is text. The short form is for the badge's Tooltip and is empty when nothing is known.
export function DriftCause({ cause, count = 1, stackName, short = false }: DriftCauseProps) {
  const t = useT();
  const text = useCauseText({ cause, count, stackName });
  if (short) return text || null;
  if (!cause) return t("stackDriftCause.outside");
  if (cause.action === "adopted") return text;
  const time = <RelativeTime value={cause.at} />;
  if (count > 1) {
    return (
      <Trans
        i18nKey="stackDriftCause.multiple"
        ns="stackDriftCause"
        count={count}
        values={{ user: cause.by }}
        components={{ time }}
      />
    );
  }
  const key =
    cause.action === "scaled" && !cause.from
      ? "stackDriftCause.scaledTo"
      : FULL_KEYS[cause.action as keyof typeof FULL_KEYS];
  return (
    <Trans
      i18nKey={key}
      ns="stackDriftCause"
      values={{ service: cause.service, from: cause.from, to: cause.to, user: cause.by }}
      components={{ time }}
    />
  );
}
