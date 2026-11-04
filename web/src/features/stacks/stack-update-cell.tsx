// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useId } from "react";

import type { StackUpdate } from "@/api/stacks";
import type { UpdateInfo } from "@/api/updates";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { UpdateBadge } from "@/features/updates/update-badge";
import { useT } from "@/i18n/use-t";

// Missing values read as a dash, never blank.
const DASH = "—";

// The reasons the server names for a workload that cannot be checked. A code a newer server adds has
// no word here and is left out rather than shown as a raw key.
const NOT_CHECKABLE = ["pinned_in_file", "not_from_registry"] as const;

// The image-shaped object UpdateBadge draws from. A stack has no image of its own: only its state
// is real, and the badge's Tooltip is given the stack's own words in its place.
function infoOf(state: Exclude<StackUpdate["state"], "none">): UpdateInfo {
  return {
    state,
    reason: "",
    registry: "",
    repository: "",
    tag: "",
    runningDigest: "",
    remoteDigest: "",
    changedPlatforms: [],
    checkedAt: "",
    successAt: "",
    note: "",
    task: false,
  };
}

interface StackUpdateCellProps {
  // The roll-up the server computed for the stack. Absent when it could not be read.
  update: StackUpdate | undefined;
}

// StackUpdateCell is the update state of a whole stack (UI-SPEC "Stack aggregate"): "{n} updates"
// with the accent icon when services have one, the aggregate state's own word otherwise, and a dash
// with the reason in a Tooltip when no service of the stack can be checked. It never reads "Up to
// date" for a stack the server did not call up to date, and a stack with no roll-up shows a dash and
// nothing else. The state is the server's; the browser works nothing out. Every word is a text node.
export function StackUpdateCell({ update }: StackUpdateCellProps) {
  const t = useT();
  const descriptionId = useId();

  if (!update) {
    return <span className="text-muted-foreground">{DASH}</span>;
  }

  if (update.state === "none") {
    const reasons = NOT_CHECKABLE.filter((r) => update.notCheckable?.includes(r))
      .map((r) => t(`updates.badge.${r}`))
      .join(", ");
    const text = reasons
      ? t("updates.stack.noneTooltip", { reasons })
      : t("updates.stack.noneTooltipBare");
    return (
      <Tooltip>
        <TooltipTrigger asChild>
          <span className="text-muted-foreground" aria-describedby={descriptionId}>
            {DASH}
          </span>
        </TooltipTrigger>
        <TooltipContent className="max-w-sm break-words">{text}</TooltipContent>
        <span id={descriptionId} className="sr-only">
          {text}
        </span>
      </Tooltip>
    );
  }

  return (
    <UpdateBadge
      info={infoOf(update.state)}
      count={update.state === "update_available" ? update.count : undefined}
      detail={t(`updates.stack.detail.${update.state}`, { count: update.count })}
    />
  );
}
