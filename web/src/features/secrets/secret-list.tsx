// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

export interface SecretListProps {
  envId: string;
  // The search text, from the URL.
  q?: string;
  onSearchChange: (q: string | undefined) => void;
}

// SecretList is the secrets list of a swarm environment. Not built yet.
export function SecretList(_props: SecretListProps): null {
  return null;
}
