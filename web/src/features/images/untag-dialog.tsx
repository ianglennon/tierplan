// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { useRef, useState } from "react";

import {
  type ImageRemoveError,
  type ImageUntagResults,
  imageUsedByQuery,
  useUntagImage,
} from "@/api/images";
import { pullTargetsQuery } from "@/api/pulls";
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
import { Label } from "@/components/ui/label";
import { RadioGroup, RadioGroupItem } from "@/components/ui/radio-group";
import { Spinner } from "@/components/ui/spinner";
import { type BulkReport, BulkResultDialog } from "@/features/containers/bulk-result-dialog";
import { inUse, nodeUse } from "@/features/images/in-use";
import { type ChooserNode, NodeChooser } from "@/features/images/node-chooser";
import { reasonOf, requestFailure } from "@/features/images/remove-image-dialog";
import { useT } from "@/i18n/use-t";
import { toastErrorWithAction, toastInfo, toastSuccess } from "@/lib/toast";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

// A node that has the image, and the tags it has there. A name can be taken off only where the
// node has it, and it is the last name when the node has no other tag.
export interface UntagNode {
  nodeId: string;
  name: string;
  tags: readonly string[];
}

interface UntagDialogProps {
  envId: string;
  imageId: string;
  // The names that can be taken off, as the image's tags show them. One is the dialog's name;
  // several let the person choose.
  names: readonly string[];
  // The nodes that have the image, each with its tags.
  nodes: readonly UntagNode[];
  // On a swarm the nodes are chosen from a list.
  swarm?: boolean;
  open: boolean;
  onClose: () => void;
  // Called when the untag went through, so the page can move on.
  onDone?: () => void;
}

// UntagDialog is "Untag {repo:tag}?" (D-29): it removes the name only, and the image stays
// under its other names, even while it is in use. Where the name is the image's only one on a
// node the untag removes the image, and the dialog says so and follows the remove rules: it is
// refused while the image is in use, saying that this is the last name. A swarm lists the nodes
// that have the name, all ticked. The confirm is the primary button: nothing here is lost that
// a pull cannot bring back by name.
export function UntagDialog({
  envId,
  imageId,
  names,
  nodes,
  swarm = false,
  open,
  onClose,
  onDone,
}: UntagDialogProps) {
  const t = useT();
  const mutation = useUntagImage(envId);
  const usage = useQuery({ ...imageUsedByQuery(envId, imageId), enabled: open });
  const targets = useQuery({ ...pullTargetsQuery(envId), enabled: open && swarm });
  const [chosenName, setChosenName] = useState<string | null>(null);
  const [picked, setPicked] = useState<string[] | null>(null);
  const [error, setError] = useState<ReturnType<typeof requestFailure> | null>(null);
  const [report, setReport] = useState<BulkReport | null>(null);
  // A second click can land before the pending state has rendered.
  const inFlight = useRef(false);
  const pending = mutation.isPending;

  const ref = chosenName && names.includes(chosenName) ? chosenName : (names[0] ?? "");
  const holders = nodes.filter((n) => n.tags.includes(ref));
  const items = usage.data?.items ?? [];
  // The last name of an image that is in use cannot be taken off on that node (D-29).
  const lastName = (n: UntagNode) => n.tags.length <= 1;
  const reasons: Record<string, string> = {};
  for (const n of holders) {
    if (lastName(n) && inUse(nodeUse(items, n.nodeId))) {
      reasons[n.nodeId] = t("images.untag.lastNameInUse");
    }
  }
  const eligible = holders.filter((n) => !reasons[n.nodeId]).map((n) => n.nodeId);
  const selected = (picked ?? eligible).filter((id) => eligible.includes(id));
  const allBlocked = holders.length > 0 && eligible.length === 0;
  const blocked = pending || allBlocked || selected.length === 0;
  // The act is a removal when every node it will run on has only this name.
  const basis = holders.filter((n) => (selected.length > 0 ? selected.includes(n.nodeId) : true));
  const removing = basis.length > 0 && basis.every(lastName);

  const chooser: ChooserNode[] = holders.map((n) => {
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

  function reset() {
    setError(null);
    setPicked(null);
    setChosenName(null);
  }

  function finish() {
    reset();
    onClose();
    onDone?.();
  }

  async function confirm() {
    if (inFlight.current || blocked) return;
    inFlight.current = true;
    setError(null);
    try {
      const results: ImageUntagResults = await mutation.mutateAsync({
        imageId,
        ref,
        nodeIds: selected,
      });
      const nodeName = (id: string | undefined) => nodes.find((n) => n.nodeId === id)?.name ?? "";
      const failed = results.filter((r) => r.result !== "done");
      const done = results.length - failed.length;
      if (failed.length === 0) {
        toastSuccess(
          results.some((r) => r.removed)
            ? t("images.untag.removed", { name: ref })
            : t("images.untag.done", { name: ref }),
        );
        finish();
      } else if (done === 0 && failed.every((r) => r.code === "not_found")) {
        toastInfo(t("images.untag.gone", { name: ref }));
        finish();
      } else if (done === 0) {
        const bad = failed[0];
        setError(
          bad
            ? reasonOf(t, ref, nodeName(bad.nodeId), bad)
            : { message: t("images.remove.errors.generic"), agentPolicy: false },
        );
      } else {
        const rows = results.map((r) => ({
          name: ref,
          node: nodeName(r.nodeId),
          result: r.result,
          reason: r.result === "done" ? "" : reasonOf(t, ref, nodeName(r.nodeId), r).message,
          code: r.code,
        }));
        toastErrorWithAction(
          t("images.untag.partial", { done, failed: failed.length }),
          t("actions.toast.details"),
          () =>
            setReport({
              verb: "remove",
              title: t("images.untag.results.title"),
              itemLabel: t("images.untag.results.image"),
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
    reset();
    onClose();
  }

  const confirmLabel = removing
    ? t("images.untag.confirmRemove")
    : t("images.untag.confirm", { name: ref });
  return (
    <>
      <AlertDialog open={open} onOpenChange={change}>
        <AlertDialogContent className="max-h-[90svh] overflow-y-auto sm:max-w-120">
          <AlertDialogHeader>
            <AlertDialogTitle className="break-all">
              {t("images.untag.title", { name: ref })}
            </AlertDialogTitle>
            <AlertDialogDescription>
              {removing ? t("images.untag.bodyOnly") : t("images.untag.body")}
            </AlertDialogDescription>
          </AlertDialogHeader>
          {names.length > 1 ? (
            <RadioGroup
              value={ref}
              onValueChange={(v) => {
                setChosenName(v);
                setPicked(null);
              }}
              aria-label={t("images.untag.chooseName")}
              className="gap-1"
            >
              {names.map((n, i) => (
                <div key={n} className="flex min-h-11 items-center gap-2 md:min-h-8">
                  <RadioGroupItem value={n} id={`untag-name-${i}`} />
                  <Label htmlFor={`untag-name-${i}`} className="font-mono text-xs break-all">
                    {n}
                  </Label>
                </div>
              ))}
            </RadioGroup>
          ) : null}
          {allBlocked ? (
            <Alert variant="warning" role="alert">
              <AlertDescription>{t("images.untag.lastNameInUse")}</AlertDescription>
            </Alert>
          ) : null}
          {swarm ? (
            <NodeChooser
              nodes={chooser}
              value={selected}
              onChange={setPicked}
              disabledReasons={reasons}
              label={t("images.untag.nodesLabel")}
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
              {t("images.untag.cancel")}
            </AlertDialogCancel>
            <Button
              type="button"
              aria-disabled={blocked}
              className={`${TOUCH_HEIGHT} aria-disabled:cursor-not-allowed aria-disabled:opacity-50`}
              onClick={() => {
                if (!blocked) void confirm();
              }}
            >
              {pending ? (
                <>
                  <Spinner aria-hidden="true" />
                  {t("images.untag.pending")}
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
