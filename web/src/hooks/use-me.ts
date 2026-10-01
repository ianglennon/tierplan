// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { components } from "@/api/schema";

type Me = components["schemas"]["MeBody"];

// RED stub: the real implementation arrives with the next commit.
export function hasPermission(_me: Me | undefined, _perm: string, _envId?: string): boolean {
  return true;
}
