// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { GLOBAL_PERMISSIONS } from "@/api/permissions.gen";

// The /api/me permissions a test stubs, laid out the way the server lays them
// out: a permission whose scope is global (internal/rbac registry) is in
// `global`, never in an environment's list. Stubs that put environments.manage,
// environments.token and the like under environments["*"] passed while the pages
// looked in that list, and hid the bug the plan 02-32 end-to-end run found: the
// real admin has those under `global`, so the Join token card, the Revoke menu and
// the Remove card never showed.

// The names the registry declares with global scope, from the file generated out
// of it, so this list cannot fall behind the registry.
const GLOBAL = new Set<string>(GLOBAL_PERMISSIONS);

export interface StubPermissions {
  global: string[];
  environments: Record<string, string[]>;
}

// splitPermissions puts each name where the server would: global-scope names in
// `global` (with any given extra ones), the rest on every environment ("*").
export function splitPermissions(names: string[], extraGlobal: string[] = []): StubPermissions {
  const global = [...new Set([...extraGlobal, ...names.filter((n) => GLOBAL.has(n))])];
  const environments = names.filter((n) => !GLOBAL.has(n));
  return { global, environments: environments.length > 0 ? { "*": environments } : {} };
}
