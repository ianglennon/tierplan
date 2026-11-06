// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useMemo, useRef, useState } from "react";

import { FLEET_BULK_MAX, type FleetSelected, useSkipFleetUpdates } from "@/api/fleet";
import { isBulkJob, useImageUpdateJobs } from "@/api/updates";
import { type BulkAction, BulkBar } from "@/features/resources/bulk-bar";
import {
  type FleetUnavailable,
  FleetUpdateConfirmDialog,
} from "@/features/updates/update-confirm-dialog";
import { hasPermission, type Me, useMe } from "@/hooks/use-me";
import { needsOf } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";

interface FleetBulkBarProps {
  // The workloads ticked on the Updates tab, in every environment they are in.
  items: readonly FleetSelected[];
  // The environments that did not report: their workloads cannot be updated now.
  notReporting: ReadonlySet<string>;
  onClear: () => void;
}

// mayUpdate is whether the caller's grants let them update this workload: services.control for both kinds
// and containers.control as well for a container. It only decides what to offer and what the confirm
// counts; the server decides.
function mayUpdate(me: Me | undefined, s: FleetSelected): boolean {
  return (
    hasPermission(me, "services.control", s.envId) &&
    (s.item.kind !== "container" || hasPermission(me, "containers.control", s.envId))
  );
}

// FleetBulkBar is the bar that takes the place of "Check now" while rows of the Updates tab are ticked
// (D-11): "{n} selected", the one accent button "Update {n} selected", "Skip selected versions" and
// "Clear selection". Update opens the confirm, which groups the ticked workloads by environment; Skip
// asks the server at once, as skipping one version does. Whatever is below a person's role is shown
// unavailable with why, and the server decides either way.
export function FleetBulkBar({ items, notReporting, onClear }: FleetBulkBarProps) {
  const t = useT();
  const me = useMe().data;
  const jobs = useImageUpdateJobs();
  const skip = useSkipFleetUpdates();
  // The workloads the confirm is showing, as they were when it opened. The tab reads again every few
  // seconds, and a row that changes under an open confirm would change what the person is about to send,
  // digests included.
  const [confirming, setConfirming] = useState<readonly FleetSelected[] | null>(null);
  const bar = useRef<HTMLDivElement>(null);

  // The environments that cannot run an update now, and why: not reporting, nothing the caller may
  // update, or another update running there. A lock held by something else is the server's to report,
  // in the answer.
  const unavailable = useMemo(() => {
    const out = new Map<string, FleetUnavailable>();
    const byEnv = new Map<string, FleetSelected[]>();
    for (const s of confirming ?? items) byEnv.set(s.envId, [...(byEnv.get(s.envId) ?? []), s]);
    for (const [envId, list] of byEnv) {
      if (notReporting.has(envId)) {
        out.set(envId, { kind: "offline" });
        continue;
      }
      if (!list.some((s) => mayUpdate(me, s))) {
        out.set(envId, { kind: "no_permission" });
        continue;
      }
      const holder = jobs.data?.find((j) => j.envId === envId && j.state === "running");
      if (holder) {
        out.set(envId, {
          kind: "locked",
          lock: isBulkJob(holder)
            ? { kind: "unknown", started: holder.startedAt, jobId: holder.id }
            : {
                kind: "update",
                workload: holder.workload,
                started: holder.startedAt,
                jobId: holder.id,
              },
        });
      }
    }
    return out;
  }, [confirming, items, notReporting, jobs.data, me]);

  // The ticked workloads in an environment that can run, which the caller may still not update (a
  // service and a container need different grants). The confirm lists them as left out, so its count
  // is what will be sent.
  const refused = useMemo(
    () => new Set((confirming ?? []).filter((s) => !unavailable.has(s.envId) && !mayUpdate(me, s))),
    [confirming, unavailable, me],
  );

  const firstEnv = items[0]?.envId;
  const updateReason =
    firstEnv !== undefined && !items.some((s) => mayUpdate(me, s))
      ? (needsOf(me, "services.control", firstEnv, t).reason ?? t("needs.role.operator"))
      : undefined;
  const skipReason = !items.some((s) => hasPermission(me, "updates.manage", s.envId))
    ? firstEnv !== undefined
      ? (needsOf(me, "updates.manage", firstEnv, t).reason ?? t("needs.role.admin"))
      : undefined
    : items.length > FLEET_BULK_MAX
      ? t("fleet.bulk.tooMany", { max: FLEET_BULK_MAX })
      : undefined;

  const actions: BulkAction[] = [
    {
      id: "update",
      label: t("fleet.bulk.update", { count: items.length }),
      primary: true,
      reason: updateReason,
      onClick: () => setConfirming(items),
    },
    {
      id: "skip",
      label: t("fleet.bulk.skip"),
      reason: skipReason,
      onClick: () => {
        if (skip.isPending) return;
        skip.mutate(items, { onSuccess: onClear });
      },
    },
  ];

  return (
    <div ref={bar} className="contents">
      <BulkBar count={items.length} actions={actions} onClear={onClear} />
      {confirming ? (
        <FleetUpdateConfirmDialog
          items={confirming}
          unavailable={unavailable}
          refused={refused}
          onClose={() => setConfirming(null)}
          onStarted={onClear}
          onClosed={() => bar.current?.querySelector("button")?.focus()}
        />
      ) : null}
    </div>
  );
}
