// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useRef, useState } from "react";

import { useDeleteVolumes, type VolumeDeleteError, type VolumeDeleteResult } from "@/api/volumes";
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
import { useT } from "@/i18n/use-t";
import { toastInfo, toastSuccess } from "@/lib/toast";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

// The volume a delete is about: the node it is on, by ID, and its name.
export interface VolumeTarget {
  name: string;
  node: string;
  nodeId: string;
}

interface DeleteVolumesDialogProps {
  envId: string;
  // The volume to delete.
  rows: readonly VolumeTarget[];
  open: boolean;
  onClose: () => void;
  // Called when the delete went through, so the list can clear what it selected.
  onDone?: () => void;
}

// What a refusal comes to: the sentence, and whether it is the agent's policy, which
// has a notice of its own.
interface Failure {
  message: string;
  agentPolicy: boolean;
}

// DeleteVolumesDialog is "Delete {name}?" (D-46): the volume and the data in it are
// gone for good, so the name is typed before the destructive button wakes up. Focus
// starts on Cancel, the dialog cannot be dismissed while the request is out, and a
// refusal stays inside it with the way forward. A volume that is already gone is said
// so in a toast and the dialog closes, since the refreshed list no longer shows it.
export function DeleteVolumesDialog({
  envId,
  rows,
  open,
  onClose,
  onDone,
}: DeleteVolumesDialogProps) {
  const t = useT();
  const mutation = useDeleteVolumes(envId);
  const [typed, setTyped] = useState("");
  const [error, setError] = useState<Failure | null>(null);
  // A second click can land before the pending state has rendered.
  const inFlight = useRef(false);
  const first = rows[0];
  const name = first?.name ?? "";
  const pending = mutation.isPending;
  const blocked = pending || !matchesName(typed, name);

  function failureOf(result: VolumeDeleteResult): Failure {
    switch (result.code) {
      case "volume_in_use":
        // The server names who uses it; the sentence is the UI's own.
        return { message: t("volumes.delete.inUse", { name }), agentPolicy: false };
      case "node_unreachable":
        return {
          message: t("volumes.errors.nodeNamedUnreachable", { node: first?.node ?? "" }),
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

  function requestFailure(err: VolumeDeleteError): Failure {
    if (err.type === "agent_policy_denied")
      return failureOf({ code: "agent_policy_denied" } as VolumeDeleteResult);
    if (err.status === 403) return { message: t("volumes.errors.permission"), agentPolicy: false };
    if (err.status === 422 && err.type === "confirmation_required") {
      return { message: t("volumes.delete.confirmation"), agentPolicy: false };
    }
    return { message: t("volumes.errors.generic"), agentPolicy: false };
  }

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
        confirm: name,
        targets: [{ nodeId: first.nodeId, name }],
      });
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
          result ? failureOf(result) : { message: t("volumes.errors.generic"), agentPolicy: false },
        );
      }
    } catch (e) {
      setError(requestFailure(e as VolumeDeleteError));
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
      <AlertDialogContent className="max-h-[90svh] overflow-y-auto sm:max-w-120">
        <AlertDialogHeader>
          {/* A long name wraps rather than running off the dialog. */}
          <AlertDialogTitle className="break-all">
            {t("volumes.delete.title", { name })}
          </AlertDialogTitle>
          <AlertDialogDescription>{t("volumes.delete.body")}</AlertDialogDescription>
        </AlertDialogHeader>
        <TypeToConfirm
          name={name}
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
            ) : (
              t("volumes.delete.confirm")
            )}
          </Button>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
