// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { queryOptions, useQuery } from "@tanstack/react-query";
import { useEffect } from "react";
import { api, setCsrfToken } from "@/api/client";
import { instanceQuery } from "@/api/instance";
import type { components } from "@/api/schema";

export type Me = components["schemas"]["MeBody"];

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

// useDocumentTitle sets the tab title to "{page} · {instance name}", or just
// the instance name when there is no page.
export function useDocumentTitle(page?: string): void {
  const { data } = useQuery(instanceQuery);
  const name = data?.instanceName ?? "";
  useEffect(() => {
    document.title = [page, name].filter(Boolean).join(" · ");
  }, [page, name]);
}
