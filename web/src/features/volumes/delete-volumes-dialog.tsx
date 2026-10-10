// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useRef, useState } from "react";

import {
  useDeleteVolumes,
  type VolumeDeleteError,
  type VolumeDeleteResult,
  type VolumeDeleteResults,
} from "@/api/volumes";
import { AgentUpdateNotice } from "@/components/agent-update-notice";
import { matchesName, TypeToConfirm } from "@/components/type-to-confirm";
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
import type { BulkReport } from "@/features/containers/bulk-result-dialog";
import { TargetList } from "@/features/resources/target-list";
import { useT } from "@/i18n/use-t";
import { toastError, toastErrorWithAction, toastInfo, toastSuccess } from "@/lib/toast";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

type Translate = (key: string, options?: Record<string, unknown>) => string;

// The volume a delete is about: the node it is on, by ID, and its name.
export interface VolumeTarget {
  name: string;
  node: string;
  nodeId: string;
}

// What a refusal comes to: the sentence, and whether it is the agent's policy, which
// has a notice of its own.
interface Failure {
  message: string;
  agentPolicy: boolean;
}

// reasonOf is the sentence for a volume that was not deleted, from its code. A code is
// never shown: the SPA words every one of them (UI-SPEC "Shared refusals").
function reasonOf(t: Translate, who: VolumeTarget, result: VolumeDeleteResult): Failure {
  switch (result.code) {
    case "volume_in_use":
      // The server names who uses it; the sentence is the UI's own.
      return { message: t("volumes.delete.inUse", { name: who.name }), agentPolicy: false };
    case "not_found":
      return { message: t("volumes.delete.gone", { name: who.name }), agentPolicy: false };
    case "node_unreachable":
      return {
        message: t("volumes.errors.nodeNamedUnreachable", { node: who.node }),
        agentPolicy: false,
      };
    case "host_read_only":
      return { message: t("volumes.errors.hostReadOnly"), agentPolicy: false };
    case "agent_policy_denied":
      return { message: t("actions.refusal.agent_policy_denied"), agentPolicy: true };
    default:
      return { message: result.detail || t("volumes.errors.generic"), agentPolicy: false };
  }
}

// requestFailure is the sentence for a delete the server refused as a whole.
function requestFailure(t: Translate, err: VolumeDeleteError): Failure {
  if (err.type === "agent_policy_denied") {
    return { message: t("actions.refusal.agent_policy_denied"), agentPolicy: true };
  }
  if (err.status === 403) return { message: t("volumes.errors.permission"), agentPolicy: false };
  if (err.status === 422 && err.type === "confirmation_required") {
    return { message: t("volumes.delete.confirmation"), agentPolicy: false };
  }
  return { message: t("volumes.errors.generic"), agentPolicy: false };
}

// reportOf lines the results up with the volumes that were asked about.
function reportOf(
  t: Translate,
  rows: readonly VolumeTarget[],
  results: VolumeDeleteResults,
): BulkReport {
  return {
    verb: "remove",
    title: t("volumes.results.title"),
    itemLabel: t("volumes.results.volume"),
    bulkId: results.bulkId,
    rows: rows.map((r, i) => {
      const result = results[i];
      if (!result) {
        return {
          name: r.name,
          node: r.node,
          result: "failed",
          reason: t("volumes.errors.generic"),
        };
      }
      return {
        name: r.name,
        node: r.node,
        result: result.result,
        reason: result.result === "done" ? "" : reasonOf(t, r, result).message,
        code: result.code,
      };
    }),
  };
}

interface DeleteVolumesDialogProps {
  envId: string;
  // The volumes to delete, one or many. Several are deleted on one confirmation, with
  // each answered on its own.
  rows: readonly VolumeTarget[];
  // On a swarm the target list says which node each one is on.
  swarm?: boolean;
  open: boolean;
  onClose: () => void;
  // Called when the delete went through, so the list can clear what it selected.
  onDone?: () => void;
  // Called with the report when the person asks for the details of a bulk that partly
  // failed.
  onDetails?: (report: BulkReport) => void;
}

// DeleteVolumesDialog is "Delete {name}?" and "Delete {n} volumes?" (D-46): the volumes
// and the data in them are gone for good, so the name is typed, or for several the
// phrase "delete {n} volumes", before the destructive button wakes up. Focus starts on
// Cancel, the dialog cannot be dismissed while the request is out, and a refusal stays
// inside it with the way forward. A single volume that is already gone is said so in a
// toast and the dialog closes, since the refreshed list no longer shows it. Several
// close when answered and sum up in one toast.
export function DeleteVolumesDialog({
  envId,
  rows,
  swarm = false,
  open,
  onClose,
  onDone,
  onDetails,
}: DeleteVolumesDialogProps) {
  const t = useT();
  const mutation = useDeleteVolumes(envId);
  const [typed, setTyped] = useState("");
  const [error, setError] = useState<Failure | null>(null);
  // A second click can land before the pending state has rendered.
  const inFlight = useRef(false);
  const first = rows[0];
  const bulk = rows.length > 1;
  const count = rows.length;
  const name = first?.name ?? "";
  // What has to be typed: the name of one volume, else the phrase for the whole selection.
  const expected = bulk ? t("volumes.delete.phrase", { count }) : name;
  const pending = mutation.isPending;
  const blocked = pending || !first || !matchesName(typed, expected);

  function reset() {
    setTyped("");
    setError(null);
  }

  async function confirm() {
    if (!first || inFlight.current || blocked) return;
    inFlight.current = true;
    setError(null);
    try {
      const results = await mutation.mutateAsync({
        confirm: expected,
        targets: rows.map((r) => ({ nodeId: r.nodeId, name: r.name })),
      });
      if (bulk) {
        sumUp(reportOf(t, rows, results));
        reset();
        onClose();
        onDone?.();
        return;
      }
      const result = results[0];
      if (result?.result === "done") {
        toastSuccess(t("volumes.delete.done", { name }));
        reset();
        onClose();
        onDone?.();
      } else if (result?.code === "not_found") {
        toastInfo(t("volumes.delete.gone", { name }));
        reset();
        onClose();
        onDone?.();
      } else {
        setError(
          result
            ? reasonOf(t, first, result)
            : { message: t("volumes.errors.generic"), agentPolicy: false },
        );
      }
    } catch (e) {
      setError(requestFailure(t, e as VolumeDeleteError));
    } finally {
      inFlight.current = false;
    }
  }

  // The one toast a bulk comes to: "Deleted 3 volumes.", or when some failed "Deleted 2,
  // 1 failed" with Details that open the results.
  function sumUp(report: BulkReport) {
    const done = report.rows.filter((r) => r.result === "done").length;
    const failed = report.rows.length - done;
    if (failed === 0) {
      toastSuccess(t("volumes.delete.doneBulk", { count: done }));
    } else if (onDetails) {
      toastErrorWithAction(
        t("volumes.delete.partial", { done, failed }),
        t("actions.toast.details"),
        () => onDetails(report),
      );
    } else {
      toastError(t("volumes.delete.partial", { done, failed }));
    }
  }

  function change(next: boolean) {
    if (next || pending) return;
    reset();
    onClose();
  }

  return (
    <AlertDialog open={open} onOpenChange={change}>
      <AlertDialogContent className="max-h-[90svh] overflow-y-auto sm:max-w-120">
        <AlertDialogHeader>
          {/* A long name wraps rather than running off the dialog. */}
          <AlertDialogTitle className="break-all">
            {bulk ? t("volumes.delete.titleBulk", { count }) : t("volumes.delete.title", { name })}
          </AlertDialogTitle>
          <AlertDialogDescription>
            {bulk ? t("volumes.delete.bodyBulk") : t("volumes.delete.body")}
          </AlertDialogDescription>
        </AlertDialogHeader>
        {bulk ? (
          <TargetList
            swarm={swarm}
            label={t("volumes.results.volume")}
            rows={rows.map((r) => ({ name: r.name, node: r.node }))}
          />
        ) : null}
        <TypeToConfirm
          name={expected}
          value={typed}
          onChange={setTyped}
          onConfirm={() => void confirm()}
          disabled={pending}
        />
        {error ? (
          error.agentPolicy ? (
            <AgentUpdateNotice envId={envId} />
          ) : (
            <Alert variant="destructive" role="alert">
              <AlertDescription>{error.message}</AlertDescription>
            </Alert>
          )
        ) : null}
        <AlertDialogFooter>
          {/* Radix focuses the Cancel button when the dialog opens. */}
          <AlertDialogCancel disabled={pending} className={TOUCH_HEIGHT}>
            {t("volumes.delete.cancel")}
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
                {t("volumes.delete.pending")}
              </>
            ) : bulk ? (
              t("volumes.delete.confirmBulk", { count })
            ) : (
              t("volumes.delete.confirm")
            )}
          </Button>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
