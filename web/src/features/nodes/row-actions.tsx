// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { ReactNode } from "react";

import type { NodeImpactAction } from "@/api/nodes";

// Every change a node's menu offers; Activate runs with no dialog.
export type NodeAction = NodeImpactAction | "activate";

// The facts of a node a menu and a pending label need.
export interface NodeActionTarget {
  id: string;
  hostname: string;
  role: string;
  availability: string;
}

export interface NodeActions {
  // The change each node is waiting on, by node ID, until the list shows it or 30 seconds pass.
  pending: ReadonlyMap<string, NodeAction>;
  // Opens the dialog of a change.
  open: (node: NodeActionTarget, action: NodeImpactAction) => void;
  // Activates a node, with no dialog.
  activate: (node: NodeActionTarget) => void;
  // The dialog to draw once, beside the menus.
  dialog: ReactNode;
}

// nodeActionsFor says which changes a node's state offers.
export function nodeActionsFor(
  _node: Pick<NodeActionTarget, "role" | "availability">,
): Record<NodeAction, boolean> {
  return { drain: false, pause: false, activate: false, promote: false, demote: false };
}

export function useNodeActions(
  _envId: string,
  _rows: readonly NodeActionTarget[] | undefined,
): NodeActions {
  return { pending: new Map(), open: () => {}, activate: () => {}, dialog: null };
}

interface NodeRowActionsProps {
  envId: string;
  node: NodeActionTarget;
  actions: NodeActions;
  // True while no manager is connected: the changes are off.
  noManager?: boolean;
  onOpenChange?: (open: boolean) => void;
}

// NodeRowActions is the menu at the end of a row of the Nodes list.
export function NodeRowActions(_props: NodeRowActionsProps) {
  return null;
}

interface NodeHeaderActionsProps {
  envId: string;
  node: NodeActionTarget;
  actions: NodeActions;
  noManager?: boolean;
}

// NodeHeaderActions are the buttons in the header of a node's page.
export function NodeHeaderActions(_props: NodeHeaderActionsProps) {
  return null;
}
