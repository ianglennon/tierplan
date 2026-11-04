// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useNavigate, useParams } from "@tanstack/react-router";
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
import { type ComponentType, type ReactNode, useId } from "react";

import type { UpdateInfo, UpdateState } from "@/api/updates";
import { useNow } from "@/components/relative-time";
import { Badge } from "@/components/ui/badge";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { shortDigest } from "@/features/tags/platform-chips";
import { canBeNamed, imageTextOf } from "@/features/updates/image-ref";
import { reasonText, rollbackReasonText } from "@/features/updates/reasons";
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
export function detailText(t: Translate, info: UpdateInfo, now: Date, name?: string): string {
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
    case "rolled_back": {
      const failure = info.failure;
      const digest = failure?.digest || info.remoteDigest;
      const who = name || t("updates.detail.workloadFallback");
      if (failure?.reason === "pull_failed") {
        // The image could not be pulled: nothing was changed, so the words never say it was rolled back.
        parts.push(
          t(digest ? "updates.detail.pull_failed" : "updates.detail.pull_failedNoDigest", {
            new: digest ? shortDigest(digest) : "",
            name: who,
            when: formatRelative(failure.at, now, t),
          }),
        );
      } else if (failure) {
        parts.push(
          t(
            digest
              ? "updates.detail.rolled_back_failure"
              : "updates.detail.rolled_back_failureNoDigest",
            {
              name: who,
              when: formatRelative(failure.at, now, t),
              reason: rollbackReasonText(t, failure.reason, {
                subject: t("updates.detail.itFallback"),
              }),
              new: digest ? shortDigest(digest) : "",
            },
          ),
        );
      } else {
        parts.push(
          info.remoteDigest
            ? t("updates.detail.rolled_back", { new: shortDigest(info.remoteDigest) })
            : t("updates.detail.rolled_back_noDigest"),
        );
      }
      break;
    }
    case "skipped": {
      const by = info.ignore;
      const digest = by?.digest || info.remoteDigest;
      if (by) {
        // Who skipped it and when (D-15); the server leaves the name empty when that person is gone.
        parts.push(
          t(digest ? "updates.detail.skipped_by" : "updates.detail.skipped_by_noDigest", {
            user: by.setBy || t("updates.detail.someone"),
            digest: digest ? shortDigest(digest) : "",
            when: formatRelative(by.setAt, now, t),
          }),
        );
      } else {
        parts.push(
          digest
            ? t("updates.detail.skipped_withDigest", { new: shortDigest(digest) })
            : t("updates.detail.skipped"),
        );
      }
      break;
    }
    case "ignored": {
      const by = info.ignore;
      if (by?.scope === "label") {
        parts.push(t("updates.detail.ignored_label"));
      } else if (by) {
        parts.push(
          t("updates.detail.ignored_by", {
            user: by.setBy || t("updates.detail.someone"),
            scope:
              by.scope === "image" && canBeNamed(info)
                ? imageTextOf(info)
                : t("updates.detail.thisWorkload"),
            when: formatRelative(by.setAt, now, t),
          }),
        );
      } else {
        parts.push(t("updates.detail.ignored"));
      }
      break;
    }
    default:
      parts.push(t(`updates.detail.${info.state}`, { checked }));
  }
  return parts.join(" ");
}

// ConsoleButton is the badge of a failed update that opens the update's console: a stack's own console
// when a stack TierPlan manages owns the workload, the update console otherwise. The router hooks live
// here and not in UpdateBadge, which is drawn in places with no router above it.
function ConsoleButton({
  jobId,
  envId,
  stackId,
  describedBy,
  className,
  children,
}: {
  jobId: string;
  envId?: string;
  stackId?: string;
  describedBy: string;
  className?: string;
  children: ReactNode;
}) {
  const navigate = useNavigate();
  const params = useParams({ strict: false, shouldThrow: false }) as { envId?: string } | undefined;
  const env = envId ?? params?.envId;
  return (
    <Badge asChild variant="outline" className={cn("cursor-pointer hover:bg-muted", className)}>
      <button
        type="button"
        aria-describedby={describedBy}
        disabled={!env}
        onClick={() => {
          if (!env) return;
          if (stackId) {
            void navigate({
              to: "/env/$envId/stacks/$stackId/deploys/$jobId",
              params: { envId: env, stackId, jobId },
            });
          } else {
            void navigate({ to: "/env/$envId/update-jobs/$jobId", params: { envId: env, jobId } });
          }
        }}
      >
        {children}
      </button>
    </Badge>
  );
}

interface UpdateBadgeProps {
  // What the server says about the image: its state, never derived here.
  info: UpdateInfo;
  // How many workloads of a stack have an update. Only an "Update available" state shows it.
  count?: number;
  // The words for the Tooltip when the caller has better ones than the image's own: the roll-up of a stack
  // is about its services, not about one image.
  detail?: string;
  // The workload's name, for the words of a rollback ("TierPlan rolled web back ..."). Absent, they say
  // "this workload".
  name?: string;
  // The environment and, for a service or container a stack TierPlan manages owns, the stack. A rolled-back
  // badge opens the console of the update that failed, and the console of a stack's update is the stack's.
  // The environment is read from the page's address when it is not given.
  envId?: string;
  stackId?: string;
  className?: string;
}

// UpdateBadge is the one place an update state is drawn (D-03): an outline badge with an icon and
// a word, except "Up to date", which is plain text with a check icon. The detail is in a Tooltip
// and, as visually hidden text referenced by aria-describedby, for a keyboard or touch reader, so
// nothing is hover-only. The icon is decorative. It is a static span, except where a failed update's
// console is still there to open (a rolled-back or failed-rollback state that names its job): then it is a
// button that opens it, and only then (UI-SPEC accessibility). Every value is drawn as a text node.
export function UpdateBadge({
  info,
  count,
  detail: detailOverride,
  name,
  envId,
  stackId,
  className,
}: UpdateBadgeProps) {
  const t = useT();
  const descriptionId = useId();
  const now = useNow();
  // An update whose image could not be pulled changed nothing, so nothing was rolled back: it has its own
  // word and its own icon, whatever state the judge files it under.
  const pullFailed = info.state === "rolled_back" && info.failure?.reason === "pull_failed";
  const { icon: Glyph, tone } = pullFailed
    ? { icon: TriangleAlert, tone: "text-status-warning-text" }
    : (STATES[info.state] ?? STATES.not_checked);
  const word = pullFailed
    ? t("updates.badge.pull_failed")
    : info.state === "update_available" && count !== undefined
      ? t("updates.badge.count", { count })
      : t(`updates.badge.${info.state}`);
  const detail = detailOverride ?? detailText(t, info, new Date(now), name);
  const jobId =
    (info.state === "rolled_back" || info.state === "rollback_failed") && info.failure
      ? info.failure.jobId
      : undefined;

  const trigger =
    info.state === "up_to_date" ? (
      <span
        className={cn("inline-flex items-center gap-1 text-xs whitespace-nowrap", className)}
        aria-describedby={descriptionId}
      >
        <Glyph className={cn("size-3", tone)} aria-hidden="true" />
        {word}
      </span>
    ) : jobId ? (
      <ConsoleButton
        jobId={jobId}
        envId={envId}
        stackId={stackId}
        describedBy={descriptionId}
        className={className}
      >
        <Glyph className={tone} aria-hidden="true" />
        {word}
      </ConsoleButton>
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
