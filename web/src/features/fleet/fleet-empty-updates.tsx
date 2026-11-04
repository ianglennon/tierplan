// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { CircleCheck, CircleDashed, TriangleAlert } from "lucide-react";
import type { ComponentType } from "react";

import type { FleetNotIncluded, FleetShowValue, FleetUpdateCoverage } from "@/api/fleet";
import { useNow } from "@/components/relative-time";
import { Button } from "@/components/ui/button";
import {
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@/components/ui/empty";
import { FleetCoverageAlert } from "@/features/fleet/fleet-coverage-alert";
import { useT } from "@/i18n/use-t";
import { formatRelative } from "@/lib/format";

type Translate = (key: string, options?: Record<string, unknown>) => string;

interface FleetEmptyUpdatesProps {
  coverage: FleetUpdateCoverage;
  // The environments the caller may read that were not read, or only in part.
  notIncluded: readonly FleetNotIncluded[] | null | undefined;
  // The Show chip the empty list was asked for. Absent: the default, Updates.
  show?: FleetShowValue;
  // How often checks run, in words ("6 hours"), when the caller knows one. Environments may each set
  // their own, so the sentence is left out when it is not given.
  interval?: string;
  // Opens another Show view: Problems after failed checks, Skipped and ignored after those.
  onShow: (show: FleetShowValue) => void;
}

interface EmptyView {
  heading: string;
  body: string;
  icon: ComponentType<{ className?: string; "aria-hidden"?: boolean | "true" | "false" }>;
  // The chip that opens the view the heading points at.
  action?: { label: string; show: FleetShowValue };
}

// firstCheckText is when the schedule next checks an environment that has workloads never checked: a
// moment away when it is due (or overdue), otherwise the local time. Empty when no schedule will.
function firstCheckText(t: Translate, iso: string, now: number): string {
  const at = Date.parse(iso);
  if (!iso || Number.isNaN(at)) return "";
  if (at - now < 60_000) return t("fleet.updates.empty.never.soon");
  return t("fleet.updates.empty.never.at", {
    time: new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeStyle: "short" }).format(
      at,
    ),
  });
}

// choose picks the words from what the data supports (D-03, UI-SPEC "Honest empty states"). A failed
// check or a workload never checked comes before everything else, since TierPlan cannot say "up to
// date" for it; skipped and ignored updates come next; an environment that did not report keeps it
// from claiming all of the fleet. "Everything is up to date" is the last resort.
function choose(
  t: Translate,
  now: number,
  coverage: FleetUpdateCoverage,
  missing: boolean,
  show: FleetShowValue,
  interval: string | undefined,
): EmptyView {
  if (show === "problems") {
    return {
      icon: CircleCheck,
      heading: t(`fleet.updates.empty.problems.${missing ? "headingPartial" : "heading"}`),
      body: t(`fleet.updates.empty.problems.${missing ? "bodyPartial" : "body"}`),
    };
  }
  if (show === "ignored") {
    return {
      icon: CircleCheck,
      heading: t(`fleet.updates.empty.ignored.${missing ? "headingPartial" : "heading"}`),
      body: t(`fleet.updates.empty.ignored.${missing ? "bodyPartial" : "body"}`),
    };
  }
  if (coverage.environments === 0) {
    return {
      icon: CircleDashed,
      heading: t("fleet.updates.empty.nothing.heading"),
      body: t("fleet.updates.empty.nothing.body"),
    };
  }
  if (coverage.failed > 0) {
    return {
      icon: TriangleAlert,
      heading: t("fleet.updates.empty.failed.heading", { count: coverage.failed }),
      body: t("fleet.updates.empty.failed.body"),
      action: { label: t("fleet.updates.empty.failed.action"), show: "problems" },
    };
  }
  if (coverage.neverChecked > 0) {
    const when = firstCheckText(t, coverage.firstCheckAt, now);
    return {
      icon: CircleDashed,
      heading: t("fleet.updates.empty.never.heading"),
      body: when
        ? t("fleet.updates.empty.never.bodyAt", { count: coverage.neverChecked, when })
        : t("fleet.updates.empty.never.body", { count: coverage.neverChecked }),
    };
  }
  if (coverage.skipped > 0) {
    return {
      icon: CircleCheck,
      heading: t("fleet.updates.empty.skipped.heading"),
      body: t("fleet.updates.empty.skipped.body", { count: coverage.skipped }),
      action: { label: t("fleet.updates.empty.skipped.action"), show: "ignored" },
    };
  }
  if (coverage.checkable === 0) {
    // Nothing follows a tag, so nothing was checked: that is not "up to date".
    return {
      icon: CircleDashed,
      heading: t("fleet.updates.empty.nothing.heading"),
      body: t("fleet.updates.empty.nothing.bodyNone"),
    };
  }
  if (missing) {
    return {
      icon: CircleCheck,
      heading: t("fleet.updates.empty.partial.heading"),
      body: t("fleet.updates.empty.partial.body"),
    };
  }
  const when = coverage.lastCheckedAt
    ? formatRelative(coverage.lastCheckedAt, new Date(now), t)
    : t("dashboard.time.missing");
  const values = {
    workloads: t("fleet.updates.empty.clean.workloads", { count: coverage.checked }),
    environments: t("fleet.updates.empty.clean.environments", { count: coverage.environments }),
    when,
    interval,
  };
  return {
    icon: CircleCheck,
    heading: t("fleet.updates.empty.clean.heading"),
    body: t(
      interval ? "fleet.updates.empty.clean.bodyInterval" : "fleet.updates.empty.clean.body",
      values,
    ),
  };
}

// FleetEmptyUpdates is the Updates tab when the list it was asked for is empty. It never says
// "Everything is up to date" unless every environment was read, every workload that follows a tag was
// checked, no check failed and nothing is merely skipped or ignored. The coverage alert for an
// environment that did not report is shown with it, so the reader sees what was left out.
export function FleetEmptyUpdates({
  coverage,
  notIncluded,
  show = "updates",
  interval,
  onShow,
}: FleetEmptyUpdatesProps) {
  const t = useT();
  const now = useNow();
  const missing = (notIncluded?.length ?? 0) > 0;
  const state = choose(t, now, coverage, missing, show, interval);
  const Icon = state.icon;
  return (
    <>
      <FleetCoverageAlert notIncluded={notIncluded} />
      <Empty className="min-h-[30svh] border">
        <EmptyHeader>
          <EmptyMedia variant="icon">
            <Icon aria-hidden="true" />
          </EmptyMedia>
          <EmptyTitle role="heading" aria-level={2} className="text-base">
            {state.heading}
          </EmptyTitle>
          <EmptyDescription>{state.body}</EmptyDescription>
        </EmptyHeader>
        {state.action ? (
          <EmptyContent>
            <Button
              variant="outline"
              size="sm"
              className="max-md:h-11"
              onClick={() => state.action && onShow(state.action.show)}
            >
              {state.action.label}
            </Button>
          </EmptyContent>
        ) : null}
      </Empty>
    </>
  );
}
