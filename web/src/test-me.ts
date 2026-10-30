// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import {
  ENVIRONMENT_PERMISSIONS,
  EXTRA_PERMISSIONS,
  GLOBAL_PERMISSIONS,
  PERMISSION_REQUIREMENTS,
  type PermissionName,
} from "@/api/permissions.gen";
import type { Me } from "@/hooks/use-me";
import type { StubPermissions } from "@/test-permissions";

// One builder for every stubbed GET /api/me, so a stub cannot disagree with the
// registry about what a role holds or which permissions are global. It reads the
// generated constants, not a hand-kept list: the Phase 2 bug, a stub that put a
// global permission where the server never does, hid a real defect until an
// end-to-end run found it. A field added to /api/me is added here once.

type EnvRole = "viewer" | "operator" | "admin";

const RANK: Record<EnvRole, number> = { viewer: 1, operator: 2, admin: 3 };

export interface MakeMeOptions {
  // An instance admin: Admin on every environment with every extra, and every
  // global permission (D-01, D-03).
  instanceAdmin?: boolean;
  username?: string;
  id?: string;
  csrfToken?: string;
  // The role the user holds on each named environment, granted directly.
  envs?: Record<string, EnvRole>;
  // The extras an Operator assignment carries, by environment (D-16, D-17).
  extras?: Record<string, string[]>;
  // A role on every environment (the "*" entry).
  allEnvironments?: EnvRole;
  // Who /api/me names for a user with no environment access. Defaults to ["admin"]
  // for such a user and to none for anyone else.
  administrators?: string[];
  // An exact permission list, for a test that needs one the roles do not give.
  permissions?: StubPermissions;
}

// permissionsOf lists the environment permissions a role holds, in registry
// order: the ones whose requirement it meets, and the extras an Admin holds
// always and an Operator holds when its assignment carries them.
function permissionsOf(role: EnvRole, extras: string[]): string[] {
  return ENVIRONMENT_PERMISSIONS.filter((name: PermissionName) => {
    const need = PERMISSION_REQUIREMENTS[name];
    if (need.kind === "instance" || !need.role) return false;
    if (RANK[role] < RANK[need.role]) return false;
    if (need.kind === "role") return true;
    return role === "admin" || extras.includes(name);
  });
}

export function makeMe(opts: MakeMeOptions = {}): Me {
  const instanceAdmin = opts.instanceAdmin === true;
  const access: Me["access"] = {};
  const environments: Record<string, string[]> = {};

  if (instanceAdmin) {
    access["*"] = {
      role: "admin",
      extras: [...EXTRA_PERMISSIONS],
      sources: [{ kind: "instance" }],
    };
    environments["*"] = [...ENVIRONMENT_PERMISSIONS];
  } else {
    if (opts.allEnvironments) {
      access["*"] = { role: opts.allEnvironments, extras: [], sources: [{ kind: "all" }] };
      environments["*"] = permissionsOf(opts.allEnvironments, []);
    }
    for (const [env, role] of Object.entries(opts.envs ?? {})) {
      const extras = opts.extras?.[env] ?? [];
      access[env] = { role, extras, sources: [{ kind: "direct" }] };
      environments[env] = permissionsOf(role, extras);
    }
  }

  const hasAccess = instanceAdmin || Object.keys(access).length > 0;
  return {
    user: {
      id: opts.id ?? (instanceAdmin ? "u1" : "u2"),
      username: opts.username ?? (instanceAdmin ? "admin" : "viewer"),
      displayName: "",
      instanceAdmin,
    },
    csrfToken: opts.csrfToken ?? "csrf-1",
    permissions: opts.permissions ?? {
      global: instanceAdmin ? [...GLOBAL_PERMISSIONS] : [],
      environments,
    },
    access,
    requirements: { ...PERMISSION_REQUIREMENTS },
    administrators: opts.administrators ?? (hasAccess ? [] : ["admin"]),
  };
}
