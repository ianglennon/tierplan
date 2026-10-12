// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// A node that has the image, and the tags it has there.
export interface UntagNode {
  nodeId: string;
  name: string;
  tags: readonly string[];
}

interface UntagDialogProps {
  envId: string;
  imageId: string;
  names: readonly string[];
  nodes: readonly UntagNode[];
  swarm?: boolean;
  open: boolean;
  onClose: () => void;
  onDone?: () => void;
}

// UntagDialog is not written yet: this is the shape its tests are written against.
export function UntagDialog(_props: UntagDialogProps) {
  return null;
}
