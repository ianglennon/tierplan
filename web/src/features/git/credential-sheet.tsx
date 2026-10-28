// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { ReactNode } from "react";

import type { GitCredential } from "@/api/git";

interface CredentialSheetProps {
  mode: "add" | "edit" | "view";
  credential?: GitCredential;
  onClose: () => void;
  // Opened over the repository sheet: only these scopes are offered, the first selected.
  scopes?: readonly string[];
  // Called with the credential that was just saved.
  onSaved?: (credential: GitCredential) => void;
}

// CredentialSheet is the right-hand panel of one git credential. Not built yet.
export function CredentialSheet(_props: CredentialSheetProps): ReactNode {
  return null;
}
