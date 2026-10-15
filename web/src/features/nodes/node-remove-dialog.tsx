// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

export interface NodeRemoveDialogProps {
  envId: string;
  // The node to remove, or null while the dialog is closed.
  node: { id: string; hostname: string } | null;
  onClose: () => void;
  // Called once the node is gone, after the dialog asked to close.
  onRemoved?: (removed: { nodeId: string }) => void;
}

// NodeRemoveDialog is the confirm of "Remove…" for a node that is down. Not built yet.
export function NodeRemoveDialog(_props: NodeRemoveDialogProps): null {
  return null;
}
