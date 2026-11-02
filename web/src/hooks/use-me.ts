// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { queryOptions, useQuery } from "@tanstack/react-query";
import { useEffect } from "react";
import { api, setCsrfToken } from "@/api/client";
import { instanceQuery } from "@/api/instance";
import type { components } from "@/api/schema";

export type Me = components["schemas"]["MeBody"];
export type Requirement = components["schemas"]["RequirementBody"];

// The ["me"] query: who is signed in, their CSRF token and their effective
// permissions. A 401 is thrown, so route guards can send the browser to /login
// and a signed-out visitor never gets cached "user" data.
export class UnauthenticatedError extends Error {
  constructor() {
    super("not signed in");
    this.name = "UnauthenticatedError";
  }
}

export const meQueryOptions = queryOptions({
  queryKey: ["me"],
  queryFn: async (): Promise<Me> => {
    const { data, response } = await api.GET("/api/me");
    if (!data) {
      if (response.status === 401) throw new UnauthenticatedError();
      throw new Error("GET /api/me failed");
    }
    // Every load, including a refresh, puts the token back in memory.
    setCsrfToken(data.csrfToken);
    return data;
  },
  retry: false,
});

export function useMe() {
  const query = useQuery(meQueryOptions);
  // The query data can also be set by sign-in; keep the token in step with it.
  const token = query.data?.csrfToken;
  useEffect(() => {
    if (token) setCsrfToken(token);
  }, [token]);
  return query;
}

// hasPermission is the pure check behind usePermission. With an environment,
// the "*" list (every environment) or that environment's own list must hold the
// permission; without one, the installation-wide list must. A global
// permission never stands in for an environment one.
export function hasPermission(me: Me | undefined, perm: string, envId?: string): boolean {
  if (!me) return false;
  if (envId === undefined) return (me.permissions.global ?? []).includes(perm);
  const envs = me.permissions.environments;
  return (envs["*"] ?? []).includes(perm) || (envs[envId] ?? []).includes(perm);
}

// isInstanceAdmin is true for an instance admin: Admin on every environment and
// the only holder of the installation-wide permissions (D-01). Like
// hasPermission it only decides what to show.
export function isInstanceAdmin(me: Me | undefined): boolean {
  return me?.user.instanceAdmin === true;
}

// hasNoEnvironmentAccess is true for a signed-in user who holds no grant on any
// environment and is not an instance admin: the one case the whole app shows the
// no-access state (D-08). Like hasPermission it only decides what to show.
export function hasNoEnvironmentAccess(me: Me | undefined): boolean {
  return me !== undefined && !isInstanceAdmin(me) && Object.keys(me.access).length === 0;
}

export function useInstanceAdmin(): boolean {
  const { data } = useMe();
  return isInstanceAdmin(data);
}

// requirementFor says what a permission asks of a caller (role, extra or
// instance), as /api/me lists it from the server's registry, so a disabled
// control can say what it needs (D-19). It is undefined for a name the server
// does not register, and while the user is not loaded.
export function requirementFor(me: Me | undefined, perm: string): Requirement | undefined {
  return me?.requirements[perm];
}

// usePermission only decides what to show. The server enforces every
// permission, so hiding a control is never the access control (D-50).
export function usePermission(perm: string, envId?: string): boolean {
  const { data } = useMe();
  return hasPermission(data, perm, envId);
}

// hasAnyEnvironmentPermission is true when the "*" list or any one environment's
// list holds perm. It is the gate for what is filtered per environment rather
// than granted globally, such as the overview: environments.read is held per
// environment, so the overview opens for anyone who may read at least one and
// the server limits its rows to those (D-49). Like hasPermission it only
// decides what to show.
export function hasAnyEnvironmentPermission(me: Me | undefined, perm: string): boolean {
  if (!me) return false;
  return Object.values(me.permissions.environments).some((list) => (list ?? []).includes(perm));
}

export function useAnyEnvironmentPermission(perm: string): boolean {
  const { data } = useMe();
  return hasAnyEnvironmentPermission(data, perm);
}

// canReadAudit is true for whoever the audit log opens for: an instance admin,
// who reads every entry, and an env Admin, who reads the entries of the
// environments they administer (D-09). Operators and Viewers hold neither. Like
// hasPermission it only decides what to show; the server scopes the read.
export function canReadAudit(me: Me | undefined): boolean {
  return (
    hasPermission(me, "audit.read") ||
    isInstanceAdmin(me) ||
    hasAnyEnvironmentPermission(me, "environments.audit")
  );
}

export function useCanReadAudit(): boolean {
  const { data } = useMe();
  return canReadAudit(data);
}

// useDocumentTitle sets the tab title to "{page} · {instance name}", or just
// the instance name when there is no page.
export function useDocumentTitle(page?: string): void {
  const { data } = useQuery(instanceQuery);
  const name = data?.instanceName ?? "";
  useEffect(() => {
    document.title = [page, name].filter(Boolean).join(" · ");
  }, [page, name]);
}
