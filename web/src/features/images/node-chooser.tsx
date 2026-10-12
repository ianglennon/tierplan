// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// A node a person can choose: the pull targets answer, as the chooser needs it.
export interface ChooserNode {
  nodeId: string;
  name: string;
  role: string;
  arch: string;
  reachable: boolean;
  readOnly?: boolean;
}

interface NodeChooserProps {
  nodes: ChooserNode[];
  value: string[];
  onChange: (ids: string[]) => void;
  disabledReasons?: Record<string, string>;
}

export function NodeChooser(_props: NodeChooserProps) {
  return null;
}
