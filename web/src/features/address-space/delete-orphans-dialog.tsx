// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQueryClient } from "@tanstack/react-query";
import { useRef, useState } from "react";

import {
  addressSpaceKey,
  deleteOrphanNetwork,
  type OrphanDelete,
  OrphanRequestError,
  type OrphanRow,
} from "@/api/address-space";
import {
  AlertDialog,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import { Progress } from "@/components/ui/progress";
import { Spinner } from "@/components/ui/spinner";
import type { BulkReport } from "@/features/containers/bulk-result-dialog";
import { TargetList } from "@/features/resources/target-list";
import { useT } from "@/i18n/use-t";
import { formatAbsolute } from "@/lib/format";
import {
  toastError,
  toastErrorWithAction,
  toastInfo,
  toastSuccess,
  toastSuccessWithAction,
} from "@/lib/toast";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

type Translate = (key: string, options?: Record<string, unknown>) => string;

// What one delete came to. "kept" is a safe answer, never an error: TierPlan checked again just
// before and left the network alone. A refusal is a failure with the words that explain it.
export type OrphanResult =
  | { kind: "deleted" }
  | { kind: "kept"; reason: string }
  | { kind: "failed"; reason: string; gone?: boolean };

// keptReason is why TierPlan kept a network, in the UI's words. node is who did not answer.
export function keptReason(
  t: Translate,
  name: string,
  reason: OrphanDelete["reason"],
  node: string,
): string {
  switch (reason) {
    case "in_use_again":
      return t("addressSpace.orphans.kept.inUseAgain", { name });
    case "node_missing":
      return t("addressSpace.orphans.kept.nodeMissing", { name, node });
    default:
      return t("addressSpace.orphans.kept.notOrphaned", { name });
  }
}

// refusalReason is the sentence for a delete the server refused or never answered. A code is
// never shown: the SPA words every one of them.
export function refusalReason(t: Translate, name: string, err: OrphanRequestError): string {
  switch (err.type) {
    case "inspect_too_large":
      return t("addressSpace.orphans.refused.inspectTooLarge", { name });
    case "network_builtin":
      return t("addressSpace.orphans.refused.builtin", { name });
    case "network_not_found":
      return t("addressSpace.orphans.refused.gone", { name });
    default:
      return err.status === 403 ? t("networks.errors.permission") : t("networks.errors.generic");
  }
}

// deleteOne sends one delete, once, and words the answer. node is who to name when the server
// could not be sure because a node did not answer.
export async function deleteOne(
  t: Translate,
  envId: string,
  row: OrphanRow,
  node: string,
): Promise<OrphanResult> {
  try {
    const answer = await deleteOrphanNetwork(
      envId,
      row.networkId,
      row.scope === "local" ? row.nodeId : undefined,
    );
    if (answer.outcome === "deleted") return { kind: "deleted" };
    return { kind: "kept", reason: keptReason(t, row.name, answer.reason, node) };
  } catch (e) {
    const err = e instanceof OrphanRequestError ? e : new OrphanRequestError(0);
    // The engine's own in-use refusal is a kept answer, however it reached us.
    if (err.type === "network_in_use") {
      return { kind: "kept", reason: keptReason(t, row.name, "in_use_again", node) };
    }
    return {
      kind: "failed",
      reason: refusalReason(t, row.name, err),
      gone: err.type === "network_not_found",
    };
  }
}

interface DeleteOrphansDialogProps {
  envId: string;
  // The networks to delete, one or many. Several are deleted on one confirmation, one at a time,
  // each judged again by the server on its own.
  rows: readonly OrphanRow[];
  open: boolean;
  swarm: boolean;
  // Who to name when a node did not answer.
  notReporting: readonly string[];
  onClose: () => void;
  // Called when the run is over, so the list can clear what it selected.
  onDone?: () => void;
  // Called with the report when the person asks for the details of a bulk.
  onDetails?: (report: BulkReport) => void;
}

// DeleteOrphansDialog is "Delete “{name}”?" and "Delete {n} networks?": the server looks again just
// before it deletes each network, saves its details in the audit log first and keeps it when
// something started using it. Focus starts on Cancel, the dialog cannot be dismissed while a
// request is out, and the answer is a toast: a kept network is information, not an error.
// Several are sent one at a time, each once, with the progress said politely at every quarter.
export function DeleteOrphansDialog({
  envId,
  rows,
  open,
  swarm,
  notReporting,
  onClose,
  onDone,
  onDetails,
}: DeleteOrphansDialogProps) {
  const t = useT();
  const queryClient = useQueryClient();
  const [progress, setProgress] = useState<{ done: number; total: number } | null>(null);
  // What the screen reader hears: only at each quarter and at the end, not at every network.
  const [announcement, setAnnouncement] = useState("");
  const lastQuarter = useRef(0);
  // A second click can land before the pending state has rendered.
  const inFlight = useRef(false);
  const pending = progress !== null;
  const bulk = rows.length > 1;
  const count = rows.length;
  const first = rows[0];
  const name = first?.name ?? "";

  function refresh() {
    void queryClient.invalidateQueries({ queryKey: addressSpaceKey(envId) });
  }

  async function remove() {
    if (!first || inFlight.current) return;
    inFlight.current = true;
    const node = (row: OrphanRow) => (notReporting.length > 0 ? notReporting.join(", ") : row.node);
    try {
      if (!bulk) {
        setProgress({ done: 0, total: 1 });
        const result = await deleteOne(t, envId, first, node(first));
        if (result.kind === "deleted") {
          toastSuccess(t("addressSpace.orphans.delete.done", { name }));
        } else if (result.kind === "kept" || result.gone) {
          toastInfo(result.reason);
        } else {
          toastError(result.reason);
        }
        refresh();
        onClose();
        onDone?.();
        return;
      }
      lastQuarter.current = 0;
      setAnnouncement("");
      const report: BulkReport["rows"] = [];
      for (const row of rows) {
        // Each network is sent once, in order. A failure is counted and the run goes on.
        setProgress({ done: report.length, total: count });
        const result = await deleteOne(t, envId, row, node(row));
        report.push({
          name: row.name,
          node: row.node,
          result:
            result.kind === "deleted" ? "done" : result.kind === "kept" ? "skipped" : "failed",
          reason: result.kind === "deleted" ? "" : result.reason,
        });
        const done = report.length;
        const quarter = Math.floor((done * 4) / count);
        if (done === count || quarter > lastQuarter.current) {
          lastQuarter.current = quarter;
          setAnnouncement(t("addressSpace.orphans.delete.progress", { done, total: count }));
        }
      }
      setProgress({ done: count, total: count });
      refresh();
      sumUp({
        verb: "remove",
        title: t("addressSpace.orphans.results.title"),
        itemLabel: t("addressSpace.orphans.results.network"),
        resultLabels: {
          done: t("addressSpace.orphans.results.deleted"),
          skipped: t("addressSpace.orphans.results.kept"),
          failed: t("addressSpace.orphans.results.failed"),
        },
        rows: report,
      });
      onClose();
      onDone?.();
    } finally {
      inFlight.current = false;
      setProgress(null);
    }
  }

  // The one toast a bulk comes to. Error styling only when something failed; a kept network is
  // information.
  function sumUp(report: BulkReport) {
    const deleted = report.rows.filter((r) => r.result === "done").length;
    const kept = report.rows.filter((r) => r.result === "skipped").length;
    const failed = report.rows.length - deleted - kept;
    if (failed === 0 && kept === 0) {
      toastSuccess(t("addressSpace.orphans.delete.doneBulk", { count: deleted }));
      return;
    }
    const message = t("addressSpace.orphans.delete.summary", { deleted, kept, failed });
    if (!onDetails) {
      if (failed > 0) toastError(message);
      else toastInfo(message);
    } else if (failed > 0) {
      toastErrorWithAction(message, t("actions.toast.details"), () => onDetails(report));
    } else {
      toastSuccessWithAction(message, t("actions.toast.details"), () => onDetails(report));
    }
  }

  function change(next: boolean) {
    if (next || pending) return;
    onClose();
  }

  return (
    <AlertDialog open={open} onOpenChange={change}>
      <AlertDialogContent className="max-h-[90svh] overflow-y-auto sm:max-w-120">
        <AlertDialogHeader>
          {/* A long name wraps rather than running off the dialog. */}
          <AlertDialogTitle className="break-words">
            {bulk
              ? t("addressSpace.orphans.delete.titleBulk", { count })
              : t("addressSpace.orphans.delete.title", { name })}
          </AlertDialogTitle>
          <AlertDialogDescription>
            {bulk
              ? t("addressSpace.orphans.delete.bodyBulk")
              : t("addressSpace.orphans.delete.body", { time: formatAbsolute(first?.idleSince) })}
          </AlertDialogDescription>
        </AlertDialogHeader>
        {bulk ? (
          <TargetList
            swarm={swarm}
            label={t("addressSpace.orphans.results.network")}
            noteLabel={t("addressSpace.orphans.delete.targetNote")}
            rows={rows.map((r) => ({
              name: r.name,
              node: r.node,
              note: (r.subnets ?? []).join(", "),
            }))}
          />
        ) : null}
        {bulk && progress ? (
          <div className="flex flex-col gap-1">
            {/* The screen reader hears the quarters below, not every network. */}
            <span aria-hidden="true" className="text-sm tabular-nums">
              {t("addressSpace.orphans.delete.progress", {
                done: progress.done,
                total: progress.total,
              })}
            </span>
            <Progress
              value={(progress.done / progress.total) * 100}
              aria-label={t("addressSpace.orphans.delete.pending")}
            />
          </div>
        ) : null}
        <span role="status" aria-live="polite" className="sr-only">
          {announcement}
        </span>
        <AlertDialogFooter>
          {/* Radix focuses the Cancel button when the dialog opens. */}
          <AlertDialogCancel disabled={pending} className={TOUCH_HEIGHT}>
            {t("addressSpace.orphans.delete.cancel")}
          </AlertDialogCancel>
          <Button
            type="button"
            aria-disabled={pending}
            className={`${TOUCH_HEIGHT} bg-destructive-fill text-white hover:bg-destructive-fill/90 aria-disabled:cursor-not-allowed aria-disabled:opacity-50`}
            onClick={() => {
              if (!pending) void remove();
            }}
          >
            {pending ? (
              <>
                <Spinner aria-hidden="true" />
                {t("addressSpace.orphans.delete.pending")}
              </>
            ) : bulk ? (
              t("addressSpace.orphans.delete.confirmBulk", { count })
            ) : (
              t("addressSpace.orphans.delete.confirm")
            )}
          </Button>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
