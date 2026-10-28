// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { ReactNode } from "react";

// The credential a delete is about: its name, and its scope picks the route family.
export interface CredentialTarget {
  id: string;
  name: string;
  scope?: { envId: string | null };
}

interface DeleteCredentialDialogProps {
  credential: CredentialTarget | null;
  open: boolean;
  onClose: () => void;
  onDone?: () => void;
}

// DeleteCredentialDialog is 'Delete "{name}"?'. Not built yet.
export function DeleteCredentialDialog(_props: DeleteCredentialDialogProps): ReactNode {
  return null;
}
