// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import {
  CircleArrowUp,
  CircleCheck,
  CircleDashed,
  CircleX,
  Clock,
  EyeOff,
  FileLock,
  History,
  Info,
  Loader2Icon,
  Lock,
  SkipForward,
  TriangleAlert,
  Undo2,
} from "lucide-react";
import { type ComponentType, useId } from "react";

import type { UpdateInfo, UpdateState } from "@/api/updates";
import { useNow } from "@/components/relative-time";
import { Badge } from "@/components/ui/badge";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { shortDigest } from "@/features/tags/platform-chips";
import { reasonText } from "@/features/updates/reasons";
import { useT } from "@/i18n/use-t";
import { formatRelative } from "@/lib/format";
import { cn } from "@/lib/utils";

type Icon = ComponentType<{ className?: string; "aria-hidden"?: boolean | "true" | "false" }>;

// The icon and its colour for each state (UI-SPEC "Update state colours"). Status colour sits on
// the icon only: "Update available" is information, so its icon takes the accent ink; a choice
// the person made (skipped, ignored) and a state nothing is wrong with are muted, never amber.
const STATES: Record<UpdateState, { icon: Icon; tone: string }> = {
  update_available: { icon: CircleArrowUp, tone: "text-accent-ink" },
  updating: { icon: Loader2Icon, tone: "text-muted-foreground motion-safe:animate-spin" },
  busy: { icon: Lock, tone: "text-muted-foreground" },
  queued: { icon: Clock, tone: "text-muted-foreground" },
  up_to_date: { icon: CircleCheck, tone: "text-status-online" },
  check_stale: { icon: History, tone: "text-muted-foreground" },
  check_failed: { icon: TriangleAlert, tone: "text-status-warning-text" },
  rolled_back: { icon: Undo2, tone: "text-status-warning-text" },
  rollback_failed: { icon: CircleX, tone: "text-status-offline-text" },
  not_from_registry: { icon: Info, tone: "text-muted-foreground" },
  pinned_in_file: { icon: FileLock, tone: "text-muted-foreground" },
  not_checked: { icon: CircleDashed, tone: "text-muted-foreground" },
  skipped: { icon: SkipForward, tone: "text-muted-foreground" },
  ignored: { icon: EyeOff, tone: "text-muted-foreground" },
};

type Translate = (key: string, options?: Record<string, unknown>) => string;

// detailText is the words behind a badge: digests, when it was checked, why a check failed. It is
// built here from the server's state and codes alone; the browser never works out a state of its
// own, and the registry's own text is not in the object it reads from.
export function detailText(t: Translate, info: UpdateInfo, now: Date): string {
  const checked = info.checkedAt ? formatRelative(info.checkedAt, now, t) : "";
  const parts: string[] = [];
  switch (info.state) {
    case "update_available": {
      const stale = info.note === "may_be_stale";
      const key = [
        "updates.detail.update_available",
        info.runningDigest ? "" : "_noRunning",
        stale ? "_stale" : "",
      ].join("");
      parts.push(
        t(key, {
          old: info.runningDigest ? shortDigest(info.runningDigest) : "",
          new: info.remoteDigest ? shortDigest(info.remoteDigest) : "",
          checked,
        }),
      );
      const platforms = (info.changedPlatforms ?? []).join(", ");
      if (platforms) parts.push(t("updates.detail.changedPlatforms", { platforms }));
      if (info.note === "running_index_gone") parts.push(t("updates.detail.running_index_gone"));
      break;
    }
    case "check_failed": {
      const reason = reasonText(t, info.reason, info.registry, info.repository, info.tag);
      parts.push(
        reason,
        info.successAt
          ? t("updates.detail.lastSuccess", { checked: formatRelative(info.successAt, now, t) })
          : t("updates.detail.neverSucceeded"),
      );
      break;
    }
    case "rolled_back":
      parts.push(
        info.remoteDigest
          ? t("updates.detail.rolled_back", { new: shortDigest(info.remoteDigest) })
          : t("updates.detail.rolled_back_noDigest"),
      );
      break;
    case "skipped":
      parts.push(
        info.remoteDigest
          ? t("updates.detail.skipped_withDigest", { new: shortDigest(info.remoteDigest) })
          : t("updates.detail.skipped"),
      );
      break;
    default:
      parts.push(t(`updates.detail.${info.state}`, { checked }));
  }
  return parts.join(" ");
}

interface UpdateBadgeProps {
  // What the server says about the image: its state, never derived here.
  info: UpdateInfo;
  // How many workloads of a stack have an update. Only an "Update available" state shows it.
  count?: number;
  // The words for the Tooltip when the caller has better ones than the image's own: the roll-up of a stack
  // is about its services, not about one image.
  detail?: string;
  className?: string;
}

// UpdateBadge is the one place an update state is drawn (D-03): an outline badge with an icon and
// a word, except "Up to date", which is plain text with a check icon. The detail is in a Tooltip
// and, as visually hidden text referenced by aria-describedby, for a keyboard or touch reader, so
// nothing is hover-only. The icon is decorative. It is a static span: nothing here opens. Every
// value is drawn as a text node.
export function UpdateBadge({ info, count, detail: detailOverride, className }: UpdateBadgeProps) {
  const t = useT();
  const descriptionId = useId();
  const now = useNow();
  const { icon: Glyph, tone } = STATES[info.state] ?? STATES.not_checked;
  const word =
    info.state === "update_available" && count !== undefined
      ? t("updates.badge.count", { count })
      : t(`updates.badge.${info.state}`);
  const detail = detailOverride ?? detailText(t, info, new Date(now));

  const trigger =
    info.state === "up_to_date" ? (
      <span
        className={cn("inline-flex items-center gap-1 text-xs whitespace-nowrap", className)}
        aria-describedby={descriptionId}
      >
        <Glyph className={cn("size-3", tone)} aria-hidden="true" />
        {word}
      </span>
    ) : (
      <Badge variant="outline" className={className} aria-describedby={descriptionId}>
        <Glyph className={tone} aria-hidden="true" />
        {word}
      </Badge>
    );

  return (
    <Tooltip>
      <TooltipTrigger asChild>{trigger}</TooltipTrigger>
      <TooltipContent className="max-w-sm break-words">{detail}</TooltipContent>
      <span id={descriptionId} className="sr-only">
        {detail}
      </span>
    </Tooltip>
  );
}
