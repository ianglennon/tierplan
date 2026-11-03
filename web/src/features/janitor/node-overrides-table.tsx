// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { RotateCcw, TriangleAlert } from "lucide-react";
import { type KeyboardEvent, useEffect, useId, useMemo, useState } from "react";

import {
  type JanitorNode,
  type JanitorNodeRequest,
  JanitorRequestError,
  useResetJanitorNode,
  useSaveJanitorNode,
} from "@/api/janitor";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Switch } from "@/components/ui/switch";
import {
  Table,
  TableBody,
  TableCaption,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { CleanupPreviewSheet } from "@/features/janitor/cleanup-preview-sheet";
import { KEEP_MAX, KEEP_MIN, parseWhole } from "@/features/janitor/policy-diff";
import { useT } from "@/i18n/use-t";
import { toastSuccess } from "@/lib/toast";

// nodeIsOn is whether the janitor cleans a node. A node is switched off only by its own setting: the
// environment's switch only stops scheduled runs, and Clean now still cleans every node that was not
// switched off. The value the server sends for a node that follows the environment is the
// environment's, so it is read only where the node set it itself.
export function nodeIsOn(node: JanitorNode): boolean {
  return !(node.enabled.overridden && !node.enabled.value);
}

// nodeBody is what a save of the node stores: a PUT replaces the node's settings, so the field that
// is not being changed is sent as it stands (its own value, or null when it follows the environment).
export function nodeBody(
  node: JanitorNode,
  patch: Partial<JanitorNodeRequest>,
): JanitorNodeRequest {
  return {
    enabled: node.enabled.overridden ? node.enabled.value : null,
    keepN: node.keepN.overridden ? node.keepN.value : null,
    ...patch,
  };
}

// couldRemoveMoreNode is whether storing `after` for a node could make the janitor remove something
// it would not remove now: switching the node back on, or keeping fewer of its task containers. A node
// that stays off removes nothing whatever its number says. A field that is null follows the
// environment, whose number is envKeepN. Reset is `after` with both fields null.
export function couldRemoveMoreNode(
  node: JanitorNode,
  after: JanitorNodeRequest,
  envKeepN: number,
): boolean {
  if (after.enabled === false) return false;
  const keep = after.keepN ?? envKeepN;
  return !nodeIsOn(node) || keep < node.keepN.value;
}

interface NodeOverridesTableProps {
  envId: string;
  envName: string;
  // The nodes of the swarm as the policy lists them. Nothing is drawn when there are none (a swarm
  // whose nodes cannot be read).
  nodes: readonly JanitorNode[] | null | undefined;
  // The environment's saved number of exited task containers kept, which a node follows until it sets
  // its own.
  envKeepN: number;
  // Whether the person holds janitor.policy; locked is what they need when they do not.
  canEdit: boolean;
  locked: string | undefined;
}

type Failure = { name: string; gone: boolean };

// What is waiting for the person to look at it before it is saved: the node and what a save would
// store for it, or null to reset it.
interface Review {
  node: JanitorNode;
  body: JanitorNodeRequest | null;
}

// NodeOverridesTable is the swarm's "Nodes" sub-section of the Policy card (D-04): a row per node with
// whether the janitor cleans it, how many exited task containers of a slot it keeps, where those
// come from, and a reset. Each node is saved on its own, at once, except a change that could make the
// janitor remove more (switching a node on, keeping fewer, or a reset that does either), which shows
// the preview of that node first (D-14, JAN-08). Without janitor.policy everything is visible and
// read-only and says what it needs (D-15).
export function NodeOverridesTable({
  envId,
  envName,
  nodes,
  envKeepN,
  canEdit,
  locked,
}: NodeOverridesTableProps) {
  const t = useT();
  const id = useId();
  const save = useSaveJanitorNode(envId);
  const reset = useResetJanitorNode(envId);
  // The text of a keep-newest field the person is typing in, by node ID. A node not in here shows
  // the number it has.
  const [typed, setTyped] = useState<Record<string, string>>({});
  const [review, setReview] = useState<Review | null>(null);
  const [failure, setFailure] = useState<Failure | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [focusNode, setFocusNode] = useState<string | null>(null);

  const switchId = (nodeId: string) => `${id}-switch-${nodeId}`;
  useEffect(() => {
    if (!focusNode) return;
    document.getElementById(switchId(focusNode))?.focus();
    setFocusNode(null);
  });

  const candidate = useMemo(
    () =>
      review
        ? { [review.node.nodeId]: review.body ?? ({ enabled: null, keepN: null } as const) }
        : undefined,
    [review],
  );

  if (!nodes || nodes.length === 0) return null;

  function forget(nodeId: string) {
    setTyped(({ [nodeId]: _gone, ...rest }) => rest);
  }

  // apply stores a node's settings (body) or resets them (null). It answers whether that was done,
  // for the preview sheet to close on; a refusal is worded under the table.
  async function apply(node: JanitorNode, body: JanitorNodeRequest | null): Promise<boolean> {
    if (!canEdit || busy) return false;
    setBusy(node.nodeId);
    setFailure(null);
    try {
      if (body) await save.mutateAsync({ nodeId: node.nodeId, body });
      else await reset.mutateAsync(node.nodeId);
      forget(node.nodeId);
      if (!body) setFocusNode(node.nodeId);
      toastSuccess(t("janitorNodes.nodes.saved", { node: node.name }));
      return true;
    } catch (e) {
      setFailure({
        name: node.name,
        gone: e instanceof JanitorRequestError && e.type === "node_not_found",
      });
      forget(node.nodeId);
      return false;
    } finally {
      setBusy(null);
    }
  }

  // ask is a change of a node: shown in the preview first when it could remove more, saved at once
  // when it cannot.
  function ask(node: JanitorNode, body: JanitorNodeRequest | null) {
    if (!canEdit || busy) return;
    const after = body ?? { enabled: null, keepN: null };
    if (couldRemoveMoreNode(node, after, envKeepN)) setReview({ node, body });
    else void apply(node, body);
  }

  function commitKeep(node: JanitorNode) {
    const text = typed[node.nodeId];
    if (text === undefined || busy) return;
    const n = parseWhole(text, KEEP_MIN, KEEP_MAX);
    // A number that is not usable stays in the field with its message; the person corrects it.
    if (n === null) return;
    if (n === node.keepN.value) {
      forget(node.nodeId);
      return;
    }
    ask(node, nodeBody(node, { keepN: n }));
  }

  function onKeyDown(node: JanitorNode, event: KeyboardEvent<HTMLInputElement>) {
    if (event.key === "Enter") {
      event.preventDefault();
      commitKeep(node);
    } else if (event.key === "Escape") {
      forget(node.nodeId);
    }
  }

  function closeReview() {
    if (review) forget(review.node.nodeId);
    setReview(null);
  }

  return (
    <section aria-labelledby={`${id}-title`} className="flex min-w-0 flex-col gap-3">
      <div className="flex flex-col gap-1">
        <h3 id={`${id}-title`} className="text-sm font-semibold">
          {t("janitorNodes.nodes.title")}
        </h3>
        <p className="text-sm text-muted-foreground">{t("janitorNodes.nodes.caption")}</p>
      </div>
      <Table>
        <TableCaption className="sr-only">{t("janitorNodes.nodes.table")}</TableCaption>
        <TableHeader>
          <TableRow>
            <TableHead scope="col" className="min-w-48">
              {t("janitorNodes.nodes.columns.node")}
            </TableHead>
            <TableHead scope="col" className="w-36">
              {t("janitorNodes.nodes.columns.cleanup")}
            </TableHead>
            <TableHead scope="col" className="w-32">
              {t("janitorNodes.nodes.columns.keep")}
            </TableHead>
            <TableHead scope="col" className="w-36">
              {t("janitorNodes.nodes.columns.state")}
            </TableHead>
            <TableHead scope="col" className="w-11">
              <span className="sr-only">{t("janitorNodes.nodes.columns.actions")}</span>
            </TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {nodes.map((node) => {
            const on = nodeIsOn(node);
            const overridden = node.enabled.overridden || node.keepN.overridden;
            const text = typed[node.nodeId];
            const shown = text ?? String(node.keepN.value);
            const keepBad = text !== undefined && parseWhole(text, KEEP_MIN, KEEP_MAX) === null;
            const keepId = `${id}-keep-${node.nodeId}`;
            const resetLabel = t("janitorNodes.nodes.reset", { node: node.name });
            const resetButton = (
              <Button
                type="button"
                variant="ghost"
                size="icon"
                className="max-md:size-11"
                aria-label={resetLabel}
                onClick={() => ask(node, null)}
              >
                <RotateCcw aria-hidden="true" />
              </Button>
            );
            return (
              <TableRow key={node.nodeId} className="h-9 max-md:h-11">
                <TableCell className="font-mono text-sm break-all">{node.name}</TableCell>
                <TableCell>
                  <div className="flex items-center gap-2">
                    <NeedsTooltip reason={locked}>
                      <Switch
                        id={switchId(node.nodeId)}
                        checked={on}
                        aria-label={t("janitorNodes.nodes.cleanupOn", { node: node.name })}
                        onCheckedChange={(next) =>
                          ask(node, nodeBody(node, { enabled: next ? null : false }))
                        }
                      />
                    </NeedsTooltip>
                    <span className="text-sm">
                      {on ? t("janitorNodes.nodes.on") : t("janitorNodes.nodes.off")}
                    </span>
                  </div>
                </TableCell>
                <TableCell>
                  <div className="flex flex-col gap-1">
                    <NeedsTooltip reason={locked}>
                      <Input
                        id={keepId}
                        type="number"
                        inputMode="numeric"
                        min={KEEP_MIN}
                        max={KEEP_MAX}
                        step={1}
                        value={shown}
                        readOnly={!canEdit}
                        aria-label={t("janitorNodes.nodes.keepOn", { node: node.name })}
                        aria-invalid={keepBad || undefined}
                        className="h-8 w-20 max-md:h-11"
                        onChange={(e) => {
                          if (!canEdit) return;
                          setTyped((prev) => ({ ...prev, [node.nodeId]: e.target.value }));
                          setFailure(null);
                        }}
                        onBlur={() => commitKeep(node)}
                        onKeyDown={(e) => onKeyDown(node, e)}
                      />
                    </NeedsTooltip>
                    {keepBad ? (
                      <p role="alert" className="text-xs text-destructive">
                        {t("janitorNodes.nodes.keepError")}
                      </p>
                    ) : null}
                  </div>
                </TableCell>
                <TableCell>
                  <div className="flex flex-wrap items-center gap-1">
                    <Badge variant="outline">
                      {overridden
                        ? t("janitorNodes.nodes.state.overridden")
                        : t("janitorNodes.nodes.state.default")}
                    </Badge>
                    {node.reporting ? null : (
                      <Badge variant="outline" className="text-status-warning-text">
                        <TriangleAlert aria-hidden="true" />
                        {t("janitorNodes.nodes.state.notReporting")}
                      </Badge>
                    )}
                  </div>
                </TableCell>
                <TableCell>
                  {overridden ? (
                    locked ? (
                      <NeedsTooltip reason={locked}>{resetButton}</NeedsTooltip>
                    ) : (
                      <Tooltip>
                        <TooltipTrigger asChild>{resetButton}</TooltipTrigger>
                        <TooltipContent>{t("janitorNodes.nodes.resetTip")}</TooltipContent>
                      </Tooltip>
                    )
                  ) : null}
                </TableCell>
              </TableRow>
            );
          })}
        </TableBody>
      </Table>
      {failure ? (
        <p role="alert" className="text-sm text-destructive">
          {failure.gone
            ? t("janitorNodes.nodes.notFound", { node: failure.name })
            : `${t("janitorNodes.nodes.saveFailed", { node: failure.name })} ${t("janitorNodes.nodes.retry")}`}
        </p>
      ) : null}

      {review ? (
        <CleanupPreviewSheet
          mode="policy"
          envId={envId}
          envName={envName}
          swarm
          nodeCount={nodes.length}
          nodes={candidate}
          onSavePolicy={() => apply(review.node, review.body)}
          onOpenChange={(open) => {
            if (!open) closeReview();
          }}
        />
      ) : null}
    </section>
  );
}
