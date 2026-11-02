// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { ChevronDown, Ellipsis } from "lucide-react";
import { type ReactNode, useCallback, useEffect, useRef, useState } from "react";

import {
  isNodeGone,
  NodeActionError,
  type NodeImpactAction,
  nodeQuery,
  useSetNodeAvailability,
} from "@/api/nodes";
import { copyText } from "@/components/copyable-code";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { StatusDot } from "@/components/status-dot";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { NodeActionDialog, nodeActionWording } from "@/features/nodes/node-action-dialog";
import { NodeRemoveDialog } from "@/features/nodes/node-remove-dialog";
import { useEnvName } from "@/features/resources/use-env-name";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { toastError, toastSuccess } from "@/lib/toast";

// How long a node reads "Draining…" at most. The row goes back to the list's own word when the
// list shows the change, or after this long, whichever comes first.
export const NODE_PENDING_MS = 30_000;

// Every change a node's menu offers; Activate runs with no dialog.
export type NodeAction = NodeImpactAction | "activate";

// The facts of a node a menu and a pending label need.
export interface NodeActionTarget {
  id: string;
  hostname: string;
  role: string;
  availability: string;
  // The manager's view of the node (ready, down, ...); Remove… needs it.
  state?: string;
}

// nodeRemovalBlock says why a node cannot be removed, or nothing when it can: a manager is
// demoted first, and only a node that is down is removed (D-22).
export function nodeRemovalBlock(
  node: Pick<NodeActionTarget, "role" | "state">,
): "manager" | "notDown" | undefined {
  if (node.role === "manager") return "manager";
  if (node.state !== "down") return "notDown";
  return undefined;
}

export interface NodeActions {
  // The change each node is waiting on, by node ID, until the list shows it or 30 seconds pass.
  pending: ReadonlyMap<string, NodeAction>;
  // Opens the dialog of a change.
  open: (node: NodeActionTarget, action: NodeImpactAction) => void;
  // Activates a node, with no dialog.
  activate: (node: NodeActionTarget) => void;
  // Opens the dialog that removes a node that is down.
  remove: (node: NodeActionTarget) => void;
  // The dialogs to draw once, beside the menus.
  dialog: ReactNode;
}

// nodeActionsFor says which changes a node's state offers: Drain… while it is not drained, Pause…
// while it is active, Activate node while it is paused or drained, Promote… for a worker and
// Demote… for a manager.
export function nodeActionsFor(
  node: Pick<NodeActionTarget, "role" | "availability">,
): Record<NodeAction, boolean> {
  const manager = node.role === "manager";
  return {
    drain: node.availability !== "drain",
    pause: node.availability === "active",
    activate: node.availability !== "active",
    promote: !manager,
    demote: manager,
  };
}

// reached says whether a node already shows the change it was asked for.
function reached(node: NodeActionTarget, action: NodeAction): boolean {
  switch (action) {
    case "drain":
      return node.availability === "drain";
    case "pause":
      return node.availability === "pause";
    case "activate":
      return node.availability === "active";
    case "promote":
      return node.role === "manager";
    case "demote":
      return node.role === "worker";
  }
}

// useNodeActions is the state behind the menus of a page of nodes: the dialog that is open, the
// Activate that has no dialog, and the change each node waits on. A node waits from the moment the
// swarm accepted a change until the rows show it, or for 30 seconds. rows are the nodes as the
// page shows them now; the same array until they change.
export function useNodeActions(
  envId: string,
  rows: readonly NodeActionTarget[] | undefined,
  options: { onRemoved?: (nodeId: string) => void } = {},
): NodeActions {
  const { onRemoved } = options;
  const t = useT();
  const queryClient = useQueryClient();
  const environment = useEnvName(envId);
  const setAvailability = useSetNodeAvailability(envId);
  const [target, setTarget] = useState<{ node: NodeActionTarget; action: NodeImpactAction } | null>(
    null,
  );
  const [removing, setRemoving] = useState<NodeActionTarget | null>(null);
  const [pending, setPending] = useState<ReadonlyMap<string, NodeAction>>(new Map());
  const timers = useRef(new Set<ReturnType<typeof setTimeout>>());

  useEffect(() => {
    const live = timers.current;
    return () => {
      for (const timer of live) clearTimeout(timer);
      live.clear();
    };
  }, []);

  const forget = useCallback((id: string, action: NodeAction) => {
    setPending((prev) => {
      if (prev.get(id) !== action) return prev;
      const next = new Map(prev);
      next.delete(id);
      return next;
    });
  }, []);

  const mark = useCallback(
    (id: string, action: NodeAction) => {
      setPending((prev) => new Map(prev).set(id, action));
      const timer = setTimeout(() => {
        timers.current.delete(timer);
        forget(id, action);
      }, NODE_PENDING_MS);
      timers.current.add(timer);
    },
    [forget],
  );

  // A node stops waiting once the rows show what it was asked for, or no longer list it.
  useEffect(() => {
    if (!rows) return;
    setPending((prev) => {
      let next: Map<string, NodeAction> | undefined;
      for (const [id, action] of prev) {
        const row = rows.find((r) => r.id === id);
        if (!row || reached(row, action)) {
          next ??= new Map(prev);
          next.delete(id);
        }
      }
      return next ?? prev;
    });
  }, [rows]);

  const open = useCallback((node: NodeActionTarget, action: NodeImpactAction) => {
    setTarget({ node, action });
  }, []);

  const remove = useCallback((node: NodeActionTarget) => setRemoving(node), []);

  const activate = useCallback(
    async (node: NodeActionTarget) => {
      mark(node.id, "activate");
      try {
        // The change names the version it was made on, so the node is read fresh first.
        const detail = await queryClient.fetchQuery({ ...nodeQuery(envId, node.id), staleTime: 0 });
        await setAvailability.mutateAsync({
          nodeId: node.id,
          availability: "active",
          version: detail.version,
        });
        toastSuccess(t("nodes.action.activate.done", { name: node.hostname }));
      } catch (e) {
        forget(node.id, "activate");
        const err = isNodeGone(e) ? new NodeActionError(404, "node_not_found") : e;
        toastError(nodeActionWording(t, err, { name: node.hostname, environment }));
      }
    },
    [mark, forget, queryClient, envId, setAvailability, t, environment],
  );

  const dialog = (
    <>
      <NodeActionDialog
        envId={envId}
        node={target?.node ?? null}
        mode={target?.action ?? "drain"}
        onClose={() => setTarget(null)}
        onDone={({ nodeId, action }) => mark(nodeId, action)}
      />
      <NodeRemoveDialog
        envId={envId}
        node={removing}
        onClose={() => setRemoving(null)}
        onRemoved={({ nodeId }) => onRemoved?.(nodeId)}
      />
    </>
  );

  return { pending, open, activate: (node) => void activate(node), remove, dialog };
}

interface ChangeItemProps {
  label: string;
  // Why the change is unavailable; absent when it is available.
  reason?: string;
  // True when the change cannot be made now, whether or not there is a reason to tell.
  blocked: boolean;
  onSelect: () => void;
  // Set when an item was chosen, so the menu does not hand focus back to its button before the
  // dialog has opened.
  chose?: { current: boolean };
  // Destructive text, for the one change that cannot be undone.
  destructive?: boolean;
}

// ChangeItem is one change in a node's menu. Unavailable it keeps its place, with the reason as a
// tooltip, since a person below Admin should see what exists (D-19). The server decides either way.
function ChangeItem({ label, reason, blocked, onSelect, chose, destructive }: ChangeItemProps) {
  return (
    <NeedsTooltip reason={blocked ? reason : undefined}>
      <DropdownMenuItem
        variant={destructive ? "destructive" : "default"}
        className="max-md:min-h-11"
        disabled={blocked && !reason}
        onSelect={() => {
          if (blocked) return;
          if (chose) chose.current = true;
          onSelect();
        }}
      >
        {label}
      </DropdownMenuItem>
    </NeedsTooltip>
  );
}

// useChangeAvailability is what the node menus share: whether the person may make changes, why
// not, and whether a manager is there to make them.
function useChangeAvailability(envId: string, noManager: boolean | undefined) {
  const t = useT();
  const needs = useNeeds("nodes.manage", envId);
  const blocked = !needs.allowed || noManager === true;
  const reason = noManager ? t("nodes.noManagerReason") : needs.reason;
  return { blocked, reason };
}

interface RemoveItemProps {
  node: NodeActionTarget;
  // True when the person may not change nodes, or nothing can be changed now, with its reason.
  blocked: boolean;
  reason: string | undefined;
  onSelect: () => void;
  chose: { current: boolean };
}

// RemoveItem is Remove… at the foot of a node's menu, in destructive text. It is offered to every
// node and unavailable with the reason when the node is a manager (demote it first) or is not
// down, after the reason a person below Admin or a swarm with no manager gives.
function RemoveItem({ node, blocked, reason, onSelect, chose }: RemoveItemProps) {
  const t = useT();
  const why = nodeRemovalBlock(node);
  const whyText =
    why === "manager"
      ? t("nodes.remove.reason.manager", { name: node.hostname })
      : why === "notDown"
        ? t("nodes.remove.reason.notDown")
        : undefined;
  return (
    <ChangeItem
      destructive
      label={t("nodes.menu.remove")}
      reason={blocked ? reason : whyText}
      blocked={blocked || why !== undefined}
      chose={chose}
      onSelect={onSelect}
    />
  );
}

interface NodeRowActionsProps {
  envId: string;
  node: NodeActionTarget;
  actions: NodeActions;
  // True while no manager is connected: the changes are off.
  noManager?: boolean;
  onOpenChange?: (open: boolean) => void;
}

// NodeRowActions is the menu at the end of a row of the Nodes list. Open goes to the node's page;
// then only the changes the node's state offers (Drain…, Pause…, Activate node, Promote…,
// Demote…). A person below Admin sees them unavailable with the reason, and so does anyone while no
// manager is connected. A node waiting on a change has its button off.
export function NodeRowActions({
  envId,
  node,
  actions,
  noManager,
  onOpenChange,
}: NodeRowActionsProps) {
  const t = useT();
  const { blocked, reason } = useChangeAvailability(envId, noManager);
  const offered = nodeActionsFor(node);
  const chose = useRef(false);
  return (
    <DropdownMenu onOpenChange={onOpenChange}>
      <DropdownMenuTrigger asChild>
        <Button
          type="button"
          variant="ghost"
          size="icon"
          className="max-md:size-11"
          disabled={actions.pending.has(node.id)}
          aria-label={t("actions.menu.label", { name: node.hostname })}
        >
          <Ellipsis aria-hidden="true" />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent
        align="end"
        onCloseAutoFocus={(event) => {
          if (!chose.current) return;
          chose.current = false;
          event.preventDefault();
        }}
      >
        <DropdownMenuItem asChild className="max-md:min-h-11">
          <Link to="/env/$envId/nodes/$nodeId" params={{ envId, nodeId: node.id }}>
            {t("actions.menu.open")}
          </Link>
        </DropdownMenuItem>
        <DropdownMenuSeparator />
        {offered.drain ? (
          <ChangeItem
            label={t("nodes.menu.drain")}
            reason={reason}
            blocked={blocked}
            chose={chose}
            onSelect={() => actions.open(node, "drain")}
          />
        ) : null}
        {offered.pause ? (
          <ChangeItem
            label={t("nodes.menu.pause")}
            reason={reason}
            blocked={blocked}
            chose={chose}
            onSelect={() => actions.open(node, "pause")}
          />
        ) : null}
        {offered.activate ? (
          <ChangeItem
            label={t("nodes.menu.activate")}
            reason={reason}
            blocked={blocked}
            onSelect={() => actions.activate(node)}
          />
        ) : null}
        {offered.promote ? (
          <ChangeItem
            label={t("nodes.menu.promote")}
            reason={reason}
            blocked={blocked}
            chose={chose}
            onSelect={() => actions.open(node, "promote")}
          />
        ) : null}
        {offered.demote ? (
          <ChangeItem
            label={t("nodes.menu.demote")}
            reason={reason}
            blocked={blocked}
            chose={chose}
            onSelect={() => actions.open(node, "demote")}
          />
        ) : null}
        <DropdownMenuSeparator />
        <RemoveItem
          node={node}
          blocked={blocked}
          reason={reason}
          chose={chose}
          onSelect={() => actions.remove(node)}
        />
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

interface NodeHeaderActionsProps {
  envId: string;
  node: NodeActionTarget;
  actions: NodeActions;
  noManager?: boolean;
}

// NodeHeaderActions are the buttons in the header of a node's page: Drain…, Pause… and Activate
// node as the node's availability allows, and under More the role change and Copy node ID. While
// the node waits on a change the header says so, with the pulsing dot, and the buttons are off.
// Unavailable ones keep their place with the reason, as in the row menu.
export function NodeHeaderActions({ envId, node, actions, noManager }: NodeHeaderActionsProps) {
  const t = useT();
  const { blocked, reason } = useChangeAvailability(envId, noManager);
  const offered = nodeActionsFor(node);
  const waiting = actions.pending.get(node.id);
  const chose = useRef(false);

  const button = (label: string, run: () => void) => (
    <NeedsTooltip reason={blocked ? reason : undefined}>
      <Button
        type="button"
        variant="outline"
        className="max-md:h-11"
        disabled={(blocked && !reason) || waiting !== undefined}
        onClick={() => {
          if (!blocked) run();
        }}
      >
        {label}
      </Button>
    </NeedsTooltip>
  );

  async function copyId() {
    if (await copyText(node.id)) toastSuccess(t("nodes.copied"));
    else toastError(t("nodes.copyFailed"));
  }

  return (
    <div className="flex flex-wrap items-center gap-2">
      {waiting ? <StatusDot state="reconnecting" label={t(`nodes.pending.${waiting}`)} /> : null}
      {offered.drain ? button(t("nodes.menu.drain"), () => actions.open(node, "drain")) : null}
      {offered.pause ? button(t("nodes.menu.pause"), () => actions.open(node, "pause")) : null}
      {offered.activate ? button(t("nodes.menu.activate"), () => actions.activate(node)) : null}
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <Button type="button" variant="outline" className="max-md:h-11">
            {t("nodes.menu.more")}
            <ChevronDown aria-hidden="true" />
          </Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent
          align="end"
          onCloseAutoFocus={(event) => {
            if (!chose.current) return;
            chose.current = false;
            event.preventDefault();
          }}
        >
          {offered.promote ? (
            <ChangeItem
              label={t("nodes.menu.promote")}
              reason={reason}
              blocked={blocked || waiting !== undefined}
              chose={chose}
              onSelect={() => actions.open(node, "promote")}
            />
          ) : null}
          {offered.demote ? (
            <ChangeItem
              label={t("nodes.menu.demote")}
              reason={reason}
              blocked={blocked || waiting !== undefined}
              chose={chose}
              onSelect={() => actions.open(node, "demote")}
            />
          ) : null}
          <DropdownMenuItem className="max-md:min-h-11" onSelect={() => void copyId()}>
            {t("nodes.menu.copyId")}
          </DropdownMenuItem>
          <DropdownMenuSeparator />
          <RemoveItem
            node={node}
            blocked={blocked || waiting !== undefined}
            reason={reason}
            chose={chose}
            onSelect={() => actions.remove(node)}
          />
        </DropdownMenuContent>
      </DropdownMenu>
    </div>
  );
}
