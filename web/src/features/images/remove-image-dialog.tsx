// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { useRef, useState } from "react";

import {
  type ImageRemoveError,
  type ImageRemoveResults,
  imageUsedByQuery,
  useRemoveImages,
} from "@/api/images";
import { pullTargetsQuery } from "@/api/pulls";
import { AgentUpdateNotice } from "@/components/agent-update-notice";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
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
import { UsedByList } from "@/components/used-by-list";
import { type BulkReport, BulkResultDialog } from "@/features/containers/bulk-result-dialog";
import { blockReason, inUseCounts, inUseWhat, nodeUse } from "@/features/images/in-use";
import { type ChooserNode, NodeChooser } from "@/features/images/node-chooser";
import { useT } from "@/i18n/use-t";
import { toastErrorWithAction, toastInfo, toastSuccess } from "@/lib/toast";

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
  result: { code?: string; detail?: string },
): Failure {
  switch (result.code) {
    case "image_in_use":
      return { message: t("images.remove.inUse", { name }), agentPolicy: false };
    case "last_name_in_use":
      return { message: t("images.remove.lastNameInUse"), agentPolicy: false };
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
  // The nodes that have the image: it is removed from the ones ticked.
  nodes: readonly ImageNodeRef[];
  // On a swarm the nodes are chosen from a list.
  swarm?: boolean;
  open: boolean;
  onClose: () => void;
  // Called when the remove went through, so the page can move on.
  onDone?: () => void;
}

// RemoveImageDialog is "Remove {ref}?" (D-28): a plain confirm, no typed name, because a
// registry image can be pulled again. On a swarm it is "Remove {ref} from nodes?" with the
// nodes that have the image ticked; a node where a container uses the image is disabled and
// says which, and a swarm service that names the image blocks every node. When nothing can
// be removed the in-use alert says what uses it and the confirm is aria-disabled with the
// count. Focus starts on Cancel, the dialog cannot be dismissed while the request is out, and
// a refusal stays inside it with the way forward. An image that is already gone is said so in
// a toast and the dialog closes, since the refreshed page no longer shows it.
export function RemoveImageDialog({
  envId,
  imageId,
  name,
  nodes,
  swarm = false,
  open,
  onClose,
  onDone,
}: RemoveImageDialogProps) {
  const t = useT();
  const mutation = useRemoveImages(envId);
  // What uses the image is read when the dialog opens, and by the server again just before
  // anything is removed. Until it answers, or if it cannot, nothing is held back here.
  const usage = useQuery({ ...imageUsedByQuery(envId, imageId), enabled: open });
  const targets = useQuery({ ...pullTargetsQuery(envId), enabled: open && swarm });
  // The nodes the person ticked; absent until they tick, which means every node that can be.
  const [picked, setPicked] = useState<string[] | null>(null);
  const [error, setError] = useState<Failure | null>(null);
  const [report, setReport] = useState<BulkReport | null>(null);
  // A second click can land before the pending state has rendered.
  const inFlight = useRef(false);
  const pending = mutation.isPending;

  const items = usage.data?.items ?? [];
  const reasons: Record<string, string> = {};
  for (const n of nodes) {
    const why = blockReason(t, nodeUse(items, n.nodeId));
    if (why) reasons[n.nodeId] = why;
  }
  const eligible = nodes.filter((n) => !reasons[n.nodeId]).map((n) => n.nodeId);
  const selected = (picked ?? eligible).filter((id) => eligible.includes(id));
  const allBlocked = nodes.length > 0 && eligible.length === 0;
  const blocked = pending || allBlocked || selected.length === 0;
  const counts = inUseCounts(items);

  const chooser: ChooserNode[] = nodes.map((n) => {
    const known = targets.data?.nodes?.find((x) => x.nodeId === n.nodeId);
    return {
      nodeId: n.nodeId,
      name: n.name,
      role: known?.role ?? "",
      arch: known?.arch ?? "",
      reachable: known?.reachable ?? true,
      readOnly: known?.readOnly,
    };
  });

  function finish() {
    setError(null);
    setPicked(null);
    onClose();
    onDone?.();
  }

  async function confirm() {
    if (inFlight.current || blocked) return;
    inFlight.current = true;
    setError(null);
    try {
      const results: ImageRemoveResults = await mutation.mutateAsync(
        selected.map((nodeId) => ({ nodeId, imageId })),
      );
      const nodeName = (id: string | undefined) => nodes.find((n) => n.nodeId === id)?.name ?? "";
      const failed = results.filter((r) => r.result !== "done");
      const done = results.length - failed.length;
      if (failed.length === 0) {
        toastSuccess(
          results.length > 1
            ? t("images.remove.doneNodes", { name, count: results.length })
            : t("images.remove.done", { name }),
        );
        finish();
      } else if (failed.every((r) => r.code === "not_found") && done === 0) {
        toastInfo(t("images.remove.gone", { name }));
        finish();
      } else if (done === 0) {
        const bad = failed[0];
        setError(
          bad
            ? reasonOf(t, name, nodeName(bad.nodeId), bad)
            : { message: t("images.remove.errors.generic"), agentPolicy: false },
        );
      } else {
        const rows = results.map((r) => ({
          name,
          node: nodeName(r.nodeId),
          result: r.result,
          reason: r.result === "done" ? "" : reasonOf(t, name, nodeName(r.nodeId), r).message,
          code: r.code,
        }));
        const summary = t("images.remove.partial", { done, failed: failed.length });
        toastErrorWithAction(summary, t("actions.toast.details"), () =>
          setReport({
            verb: "remove",
            title: t("images.remove.results.title"),
            itemLabel: t("images.remove.results.image"),
            bulkId: results.bulkId,
            rows,
          }),
        );
        finish();
      }
    } catch (e) {
      setError(requestFailure(t, e as ImageRemoveError));
    } finally {
      inFlight.current = false;
    }
  }

  function change(next: boolean) {
    if (next || pending) return;
    setError(null);
    setPicked(null);
    onClose();
  }

  const confirmLabel = allBlocked
    ? t("images.remove.inUseConfirm", { what: inUseWhat(t, counts.containers, counts.services) })
    : swarm
      ? t("images.remove.confirmNodes", { selected: selected.length, total: nodes.length })
      : t("images.remove.confirm");

  return (
    <>
      <AlertDialog open={open} onOpenChange={change}>
        <AlertDialogContent className="max-h-[90svh] overflow-y-auto sm:max-w-120">
          <AlertDialogHeader>
            {/* A long ref wraps rather than running off the dialog. */}
            <AlertDialogTitle className="break-all">
              {t(swarm ? "images.remove.titleSwarm" : "images.remove.title", { name })}
            </AlertDialogTitle>
            <AlertDialogDescription>{t("images.remove.body")}</AlertDialogDescription>
          </AlertDialogHeader>
          {allBlocked ? (
            <Alert variant="warning">
              <AlertTitle className="break-all">{t("usedBy.image.title", { name })}</AlertTitle>
              <AlertDescription className="flex flex-col gap-2">
                <p>{t("usedBy.image.body")}</p>
                <UsedByList
                  containers={items
                    .filter((u) => u.kind === "container")
                    .map((u) => ({ name: u.name, node: u.node }))}
                  services={items
                    .filter((u) => u.kind === "service")
                    .map((u) => ({ name: u.name }))}
                />
              </AlertDescription>
            </Alert>
          ) : null}
          {swarm ? (
            <NodeChooser
              nodes={chooser}
              value={selected}
              onChange={setPicked}
              disabledReasons={reasons}
              label={t("images.remove.nodesLabel")}
            />
          ) : null}
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
                confirmLabel
              )}
            </Button>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
      <BulkResultDialog envId={envId} report={report} onClose={() => setReport(null)} />
    </>
  );
}
