// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { ReactNode } from "react";

import type { GitCredential } from "@/api/git";
import type { GitFilters, GitFiltersPatch, ScopeOption } from "@/features/git/git-table-parts";

interface CredentialTableProps {
  credentials: readonly GitCredential[] | undefined;
  isPending: boolean;
  isError: boolean;
  isFetching: boolean;
  refetch: () => unknown;
  filters: GitFilters;
  onFiltersChange: (patch: GitFiltersPatch) => void;
  scopes: readonly ScopeOption[];
  canAdd: boolean;
  banner: ReactNode;
  onAdd: () => void;
  onOpen: (id: string) => void;
  onDelete: (credential: GitCredential) => void;
  // The names of the repositories that use each credential, by credential ID.
  usedByNames: ReadonlyMap<string, readonly string[]>;
}

// CredentialTable is the Credentials tab. Not built yet.
export function CredentialTable(_props: CredentialTableProps): ReactNode {
  return null;
}
