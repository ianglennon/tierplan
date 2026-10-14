// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { NodeImpactAction } from "@/api/nodes";

export interface NodeActionDialogProps {
  envId: string;
  // The node the change is for, or null while the dialog is closed.
  node: { id: string; hostname: string } | null;
  mode: NodeImpactAction;
  onClose: () => void;
  // Called once the swarm has accepted the change, after the dialog asked to close.
  onDone?: (change: { nodeId: string; action: NodeImpactAction }) => void;
}

// NodeActionDialog is the dialog of Drain, Pause, Promote and Demote.
export function NodeActionDialog(_props: NodeActionDialogProps) {
  return null;
}

// nodeActionWording words a change to a node that did not happen.
export function nodeActionWording(
  _t: (key: string) => string,
  _err: unknown,
  _ctx: { name: string; environment: string },
): string {
  return "";
}
