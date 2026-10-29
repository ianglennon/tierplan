// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { ArrowDown, CircleX } from "lucide-react";
import { useId } from "react";

import type { StackGit } from "@/api/stack-git";
import { Badge } from "@/components/ui/badge";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { reasonOfGit } from "@/features/stack-git/sync-status";
import { useT } from "@/i18n/use-t";

interface BadgeProps {
  // Where the stack stands against its repository, as the list carries it; absent for a stack that is not from git.
  git: StackGit | undefined;
}

// BehindBadge marks a git stack whose repository folder has commits that are not deployed (UI-SPEC "Stack lists"). It is
// an outline badge with a warning arrow after the stack's name, and draws nothing for a stack that is in sync, not
// checked yet or not from git. A check that failed keeps the count it last made, so a stack in error that was behind
// still says so. A stack behind because its source changed has no count and says only "Behind". The Tooltip's words
// are also the badge's description, for a reader who never opens it.
export function BehindBadge({ git }: BadgeProps) {
  const t = useT();
  const descriptionId = useId();
  if (!git || git.behind <= 0 || (git.status !== "behind" && git.status !== "error")) return null;
  const sourceChanged = git.status === "behind" && git.reasonCode === "source_changed";
  const label = sourceChanged
    ? t("stackGit.badge.behindNoCount")
    : t(git.behindCapped ? "stackGit.badge.behindCapped" : "stackGit.badge.behind", {
        n: git.behind,
      });
  const tip = sourceChanged
    ? t("stackGit.badge.behindSourceTip")
    : t(git.behindCapped ? "stackGit.badge.behindTipCapped" : "stackGit.badge.behindTip", {
        count: git.behind,
      });
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <Badge variant="outline" className="shrink-0" aria-describedby={descriptionId}>
          <ArrowDown className="text-status-warning-text" aria-hidden="true" />
          {label}
        </Badge>
      </TooltipTrigger>
      <TooltipContent className="max-w-sm break-words">{tip}</TooltipContent>
      <span id={descriptionId} className="sr-only">
        {tip}
      </span>
    </Tooltip>
  );
}

// SyncErrorBadge marks a git stack whose last sync check could not be made. Its Tooltip gives the reason in the words
// of the Sync error reasons, and falls back to the server's own sentence for a code this build has no words for. The
// reason is text, never markup.
export function SyncErrorBadge({ git }: BadgeProps) {
  const t = useT();
  const descriptionId = useId();
  if (git?.status !== "error") return null;
  const tip = reasonOfGit(t, git) ?? git.reasonDetail ?? t("stackGit.badge.errorTip");
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <Badge variant="outline" className="shrink-0" aria-describedby={descriptionId}>
          <CircleX className="text-status-offline-text" aria-hidden="true" />
          {t("stackGit.badge.syncError")}
        </Badge>
      </TooltipTrigger>
      <TooltipContent className="max-w-sm break-words">{tip}</TooltipContent>
      <span id={descriptionId} className="sr-only">
        {tip}
      </span>
    </Tooltip>
  );
}

// GitSourceBadge is the outline "Git" badge of a stack made from a repository, in the header of its page and in the
// Source column of the lists, with the repository and the ref in its Tooltip and as its description. Without the
// summary (a read that has not filled it) it is the plain badge.
export function GitSourceBadge({ git }: BadgeProps) {
  const t = useT();
  const descriptionId = useId();
  const badge = (
    <Badge variant="outline" aria-describedby={git ? descriptionId : undefined}>
      {t("stacks.source.git")}
    </Badge>
  );
  if (!git) return badge;
  const tip = t("stackGit.sourceTip", { repo: git.repoName, ref: git.ref });
  return (
    <Tooltip>
      <TooltipTrigger asChild>{badge}</TooltipTrigger>
      <TooltipContent className="max-w-sm break-words">{tip}</TooltipContent>
      <span id={descriptionId} className="sr-only">
        {tip}
      </span>
    </Tooltip>
  );
}
