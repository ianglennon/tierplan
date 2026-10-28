// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { ReactNode } from "react";

// The repository a delete is about: its name, and its scope picks the route family.
export interface RepositoryTarget {
  id: string;
  name: string;
  scope?: { envId: string | null };
}

interface DeleteRepositoryDialogProps {
  repository: RepositoryTarget | null;
  open: boolean;
  onClose: () => void;
  onDone?: () => void;
}

// DeleteRepositoryDialog is 'Delete "{name}"?'. Not built yet.
export function DeleteRepositoryDialog(_props: DeleteRepositoryDialogProps): ReactNode {
  return null;
}
