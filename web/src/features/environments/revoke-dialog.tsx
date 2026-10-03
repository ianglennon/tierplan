// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { AgentItem } from "@/api/agents";

interface RevokeDialogProps {
  envId: string;
  agent: AgentItem | null;
  onClose: () => void;
  onRotateToken?: () => void;
}

export function RevokeDialog(_props: RevokeDialogProps) {
  return null;
}

interface UnblockDialogProps {
  envId: string;
  agent: AgentItem | null;
  onClose: () => void;
}

export function UnblockDialog(_props: UnblockDialogProps) {
  return null;
}
