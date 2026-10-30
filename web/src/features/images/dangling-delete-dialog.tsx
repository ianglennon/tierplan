// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useRef, useState } from "react";

import {
  type ImageRemoveError,
  type ImageRemoveResult,
  type ImageRemoveResults,
  useRemoveImages,
} from "@/api/images";
import { AgentUpdateNotice } from "@/components/agent-update-notice";
import { Alert, AlertDescription } from "@/components/ui/alert";
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
import { Spinner } from "@/components/ui/spinner";
import type { BulkReport, BulkReportRow } from "@/features/containers/bulk-result-dialog";
import { usePendingApi } from "@/features/containers/pending";
import { reasonOf, requestFailure } from "@/features/images/remove-image-dialog";
import { TargetList } from "@/features/resources/target-list";
import { useT } from "@/i18n/use-t";
import {
  toastError,
  toastErrorWithAction,
  toastSuccess,
  toastSuccessWithAction,
} from "@/lib/toast";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

// A node an image is on. inUse is set for a copy something uses: it is listed, muted, and
// never sent.
export interface DanglingNode {
  nodeId: string;
  name: string;
  inUse?: boolean;
}

// One selected row of the list: the image, by its full ID and the short one the list shows,
// the nodes it is on, and the key of its row so the list can mark it Removing….
export interface DanglingTarget {
  rowKey: string;
  imageId: string;
  shortId: string;
  nodes: readonly DanglingNode[];
  // A string that differs once the removal has taken effect on the row, for the pending mark.
  fingerprint: string;
}

interface Failure {
  message: string;
  agentPolicy: boolean;
}

// What the person is told about one target. In use, and a name found by the server, both mean
// the target was left alone, so both are "skipped" and neither is an error (D-17).
function outcomeOf(r: ImageRemoveResult | undefined): BulkReportRow["result"] {
  if (!r) return "failed";
  if (r.result === "done") return "done";
  if (r.result === "skipped" || r.code === "image_in_use" || r.code === "not_dangling") {
    return "skipped";
  }
  return "failed";
}

interface DanglingDeleteDialogProps {
  envId: string;
  // The selected rows. Several images are deleted on one confirmation, each answered on its
  // own, in chunks of 25 that share one bulk ID.
  rows: readonly DanglingTarget[];
  // On a swarm the target list says which node each copy is on.
  swarm?: boolean;
  open: boolean;
  onClose: () => void;
  // Called when the delete went through, so the list can clear what it selected.
  onDone?: () => void;
  // Called with the report when the person asks for the details of a bulk that partly
  // failed or skipped.
  onDetails?: (report: BulkReport) => void;
}

// DanglingDeleteDialog is "Delete {n} images?" (D-17): untagged images nothing uses, deleted
// on one plain confirmation (no typed phrase: an image can be pulled again, and the body says
// the ones built here cannot). The TargetList names every image and node. A copy something
// uses is muted with "In use, skipped" and not sent; the server refuses a named or used one
// whatever is sent (T-05-58). The send goes out in chunks of BULK_CHUNK (25) sharing one bulk ID
// (useRemoveImages does the chunking), and the report Details opens is the list's
// BulkResultDialog. Focus starts on Cancel, the dialog cannot be dismissed while the
// request is out, and a refusal of the whole request stays inside it. Otherwise it closes and
// one toast sums up: "Deleted {n} images.", or what was removed, failed and skipped with
// Details that open the results.
export function DanglingDeleteDialog({
  envId,
  rows,
  swarm = false,
  open,
  onClose,
  onDone,
  onDetails,
}: DanglingDeleteDialogProps) {
  const t = useT();
  const mutation = useRemoveImages(envId);
  const pendingApi = usePendingApi();
  const [error, setError] = useState<Failure | null>(null);
  // A second click can land before the pending state has rendered.
  const inFlight = useRef(false);
  const pending = mutation.isPending;

  // One line per image copy, in the order of the rows; a copy in use is shown, not sent.
  const copies = rows.flatMap((r) =>
    r.nodes.map((n) => ({ row: r, node: n, send: !n.inUse, label: r.shortId })),
  );
  const sent = copies.filter((c) => c.send);
  const count = sent.length;
  const blocked = pending || count === 0;

  function reset() {
    setError(null);
  }

  // reportOf lines the answers up with the copies, in the order they were asked about.
  function reportOf(results: ImageRemoveResults): BulkReport {
    let at = 0;
    return {
      verb: "remove",
      title: t("images.bulk.results.title"),
      itemLabel: t("images.bulk.results.image"),
      bulkId: results.bulkId,
      rows: copies.map((c): BulkReportRow => {
        if (!c.send) {
          return {
            name: c.label,
            node: c.node.name,
            result: "skipped",
            reason: t("images.bulk.skippedNote"),
          };
        }
        const r = results[at];
        at += 1;
        const result = outcomeOf(r);
        let reason = "";
        if (result === "skipped") reason = t("images.bulk.skippedNote");
        else if (result === "failed") {
          reason = r
            ? reasonOf(t, c.label, c.node.name, r).message
            : t("images.remove.errors.generic");
        }
        return { name: c.label, node: c.node.name, result, reason, code: r?.code };
      }),
    };
  }

  // The one toast a bulk comes to.
  function sumUp(report: BulkReport) {
    const done = report.rows.filter((r) => r.result === "done").length;
    const skipped = report.rows.filter((r) => r.result === "skipped").length;
    const failed = report.rows.length - done - skipped;
    if (failed === 0 && skipped === 0) {
      toastSuccess(t("images.bulk.done", { count: done }));
      return;
    }
    const message =
      failed === 0
        ? t("images.bulk.skippedOnly", { done, skipped })
        : skipped === 0
          ? t("images.bulk.partial", { done, failed })
          : t("images.bulk.partialSkipped", { done, failed, skipped });
    if (!onDetails) {
      if (failed === 0) toastSuccess(message);
      else toastError(message);
      return;
    }
    const show = () => onDetails(report);
    if (failed === 0) toastSuccessWithAction(message, t("actions.toast.details"), show);
    else toastErrorWithAction(message, t("actions.toast.details"), show);
  }

  async function confirm() {
    if (inFlight.current || blocked) return;
    inFlight.current = true;
    setError(null);
    // Every targeted row says Removing… until the list shows it differently.
    const marks = rows.map((r) => ({ key: r.rowKey, name: r.shortId, fingerprint: r.fingerprint }));
    pendingApi.mark(marks, "remove");
    try {
      const results = await mutation.mutateAsync(
        sent.map((c) => ({ nodeId: c.node.nodeId, imageId: c.row.imageId })),
      );
      const report = reportOf(results);
      // A row nothing was removed from is what it was: it loses its Removing… label at once.
      // The others lose it when the list shows them differently.
      const removedFrom = new Set<string>();
      let at = 0;
      for (const c of copies) {
        if (!c.send) continue;
        if (results[at]?.result === "done") removedFrom.add(c.row.rowKey);
        at += 1;
      }
      pendingApi.release(rows.filter((r) => !removedFrom.has(r.rowKey)).map((r) => r.rowKey));
      sumUp(report);
      reset();
      onClose();
      onDone?.();
    } catch (e) {
      pendingApi.release(marks.map((m) => m.key));
      const failure = requestFailure(t, e as ImageRemoveError);
      setError(failure);
    } finally {
      inFlight.current = false;
    }
  }

  function change(next: boolean) {
    if (next || pending) return;
    reset();
    onClose();
  }

  return (
    <AlertDialog open={open} onOpenChange={change}>
      <AlertDialogContent className="max-h-[90svh] overflow-y-auto sm:max-w-140">
        <AlertDialogHeader>
          <AlertDialogTitle>{t("images.bulk.title", { count })}</AlertDialogTitle>
          <AlertDialogDescription>{t("images.bulk.body")}</AlertDialogDescription>
        </AlertDialogHeader>
        <TargetList
          swarm={swarm}
          label={t("images.bulk.column")}
          rows={copies.map((c) => ({
            name: c.label,
            node: c.node.name,
            skipped: !c.send,
            note: c.send ? undefined : t("images.bulk.skippedNote"),
          }))}
        />
        {error ? (
          error.agentPolicy ? (
            <AgentUpdateNotice envId={envId} />
          ) : (
            <Alert variant="destructive" role="alert">
              <AlertDescription className="break-words">{error.message}</AlertDescription>
            </Alert>
          )
        ) : null}
        <AlertDialogFooter>
          {/* Radix focuses the Cancel button when the dialog opens. */}
          <AlertDialogCancel disabled={pending} className={TOUCH_HEIGHT}>
            {t("images.bulk.cancel")}
          </AlertDialogCancel>
          <Button
            type="button"
            aria-disabled={blocked}
            className={`${TOUCH_HEIGHT} bg-destructive-fill text-white hover:bg-destructive-fill/90 aria-disabled:cursor-not-allowed aria-disabled:opacity-50`}
            onClick={() => {
              if (!blocked) void confirm();
            }}
          >
            {pending ? (
              <>
                <Spinner aria-hidden="true" />
                {t("images.bulk.pending")}
              </>
            ) : (
              t("images.bulk.confirm", { count })
            )}
          </Button>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
