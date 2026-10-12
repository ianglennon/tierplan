// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

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
import { useT } from "@/i18n/use-t";
import { toastInfo, toastSuccess } from "@/lib/toast";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

type Translate = (key: string, options?: Record<string, unknown>) => string;

// A node the image is on, by ID and display name.
export interface ImageNodeRef {
  nodeId: string;
  name: string;
}

// What a refusal comes to: the sentence, and whether it is the agent's policy, which has a
// notice of its own.
interface Failure {
  message: string;
  agentPolicy: boolean;
}

// reasonOf is the sentence for a copy that was not removed, from its code. A code is never
// shown: the SPA words every one of them (UI-SPEC "Shared refusals").
export function reasonOf(
  t: Translate,
  name: string,
  node: string,
  result: ImageRemoveResult,
): Failure {
  switch (result.code) {
    case "image_in_use":
      return { message: t("images.remove.inUse", { name }), agentPolicy: false };
    case "not_found":
      return { message: t("images.remove.gone", { name }), agentPolicy: false };
    case "node_unreachable":
      return {
        message: t("images.remove.errors.nodeNamedUnreachable", { node }),
        agentPolicy: false,
      };
    case "host_read_only":
      return { message: t("images.remove.errors.hostReadOnly"), agentPolicy: false };
    case "agent_policy_denied":
      return { message: t("actions.refusal.agent_policy_denied"), agentPolicy: true };
    default:
      return { message: result.detail || t("images.remove.errors.generic"), agentPolicy: false };
  }
}

// requestFailure is the sentence for a remove the server refused as a whole.
export function requestFailure(t: Translate, err: ImageRemoveError): Failure {
  if (err.type === "agent_policy_denied") {
    return { message: t("actions.refusal.agent_policy_denied"), agentPolicy: true };
  }
  if (err.status === 403) {
    return { message: t("images.remove.errors.permission"), agentPolicy: false };
  }
  return { message: t("images.remove.errors.generic"), agentPolicy: false };
}

interface RemoveImageDialogProps {
  envId: string;
  imageId: string;
  // What the image is called in the title and the toasts: its first tag, else its short ID.
  name: string;
  // The nodes the image is removed from.
  nodes: readonly ImageNodeRef[];
  // On a swarm the nodes are chosen from a list.
  swarm?: boolean;
  open: boolean;
  onClose: () => void;
  // Called when the remove went through, so the page can move on.
  onDone?: () => void;
}

// RemoveImageDialog is "Remove {ref}?" (D-28): a plain confirm, no typed name, because a
// registry image can be pulled again. Focus starts on Cancel, the dialog cannot be dismissed
// while the request is out, and a refusal stays inside it with the way forward. An image that
// is already gone is said so in a toast and the dialog closes, since the refreshed page no
// longer shows it.
export function RemoveImageDialog({
  envId,
  imageId,
  name,
  nodes,
  open,
  onClose,
  onDone,
}: RemoveImageDialogProps) {
  const t = useT();
  const mutation = useRemoveImages(envId);
  const [error, setError] = useState<Failure | null>(null);
  // A second click can land before the pending state has rendered.
  const inFlight = useRef(false);
  const pending = mutation.isPending;
  const blocked = pending || nodes.length === 0;

  function finish() {
    setError(null);
    onClose();
    onDone?.();
  }

  async function confirm() {
    if (inFlight.current || blocked) return;
    inFlight.current = true;
    setError(null);
    try {
      const results: ImageRemoveResults = await mutation.mutateAsync(
        nodes.map((n) => ({ nodeId: n.nodeId, imageId })),
      );
      const failed = results.findIndex((r) => r.result !== "done");
      if (failed < 0) {
        toastSuccess(t("images.remove.done", { name }));
        finish();
        return;
      }
      const bad = results[failed];
      if (bad?.code === "not_found" && results.every((r) => r.code === "not_found")) {
        toastInfo(t("images.remove.gone", { name }));
        finish();
        return;
      }
      const node = nodes.find((n) => n.nodeId === bad?.nodeId)?.name ?? "";
      setError(
        bad
          ? reasonOf(t, name, node, bad)
          : { message: t("images.remove.errors.generic"), agentPolicy: false },
      );
    } catch (e) {
      setError(requestFailure(t, e as ImageRemoveError));
    } finally {
      inFlight.current = false;
    }
  }

  function change(next: boolean) {
    if (next || pending) return;
    setError(null);
    onClose();
  }

  return (
    <AlertDialog open={open} onOpenChange={change}>
      <AlertDialogContent className="max-h-[90svh] overflow-y-auto sm:max-w-120">
        <AlertDialogHeader>
          {/* A long ref wraps rather than running off the dialog. */}
          <AlertDialogTitle className="break-all">
            {t("images.remove.title", { name })}
          </AlertDialogTitle>
          <AlertDialogDescription>{t("images.remove.body")}</AlertDialogDescription>
        </AlertDialogHeader>
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
            {t("images.remove.cancel")}
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
                {t("images.remove.pending")}
              </>
            ) : (
              t("images.remove.confirm")
            )}
          </Button>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
