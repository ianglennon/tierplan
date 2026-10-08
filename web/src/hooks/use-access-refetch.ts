// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { QueryClient } from "@tanstack/react-query";
import { useQueryClient } from "@tanstack/react-query";
import { useRouter } from "@tanstack/react-router";
import { useEffect, useRef, useSyncExternalStore } from "react";

import { type AccessChangeKind, setAccessChangeHandler } from "@/api/client";
import { environmentsQuery, recentQuery } from "@/api/environments";
import { hasNoEnvironmentAccess, type Me, meQueryOptions } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { stripBase } from "@/lib/base-path";
import { SUCCESS_TOAST_MS, toastError, toastInfo } from "@/lib/toast";

// At most one /api/me refetch in this long, however many requests are refused
// (T-03-100). There is no polling and no stream for access changes: only a 403
// or 404 the app was going to see anyway sets this off (D-10).
export const ACCESS_REFETCH_THROTTLE_MS = 5000;

// While the refetch and the move run, the environment page shows its skeleton:
// a page that just answered 404 must not flash "Page not found" or the no-access
// state before the app knows where the user belongs now.
let pending = 0;
const listeners = new Set<() => void>();

function setPending(delta: number): void {
  pending += delta;
  for (const listener of listeners) listener();
}

export function isAccessRefetching(): boolean {
  return pending > 0;
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

// useAccessRefetching is true while the app is finding out what a refused
// request means for the user.
export function useAccessRefetching(): boolean {
  return useSyncExternalStore(subscribe, isAccessRefetching, isAccessRefetching);
}

export interface AccessChangeDeps {
  queryClient: QueryClient;
  // The environment the user is looking at, from the route, if any.
  currentEnvId: () => string | undefined;
  // Opens an environment; undefined goes home, where the no-access state shows.
  moveTo: (envId: string | undefined) => void;
  // "Your access changed. Showing what you can see now."
  notifyChanged: () => void;
  // "You no longer have permission to do that."
  notifyRefused: () => void;
  now?: () => number;
}

// Whether the user may open this environment, as /api/me says. The "*" entry is
// access to every environment.
function mayOpen(me: Me, envId: string): boolean {
  return me.access["*"] !== undefined || me.access[envId] !== undefined;
}

// What the user can do, for telling whether a refetch brought news.
function accessKey(me: Me | undefined): string {
  return me ? JSON.stringify({ access: me.access, permissions: me.permissions }) : "";
}

// Where a user who lost their environment goes: the last one they used that they
// can still open, else the first they can open, else home (D-10).
async function nextEnvironment(queryClient: QueryClient): Promise<string | undefined> {
  const list = await queryClient.fetchQuery({ ...environmentsQuery, staleTime: 0 });
  const ids = (list.items ?? []).map((e) => e.id);
  const recent = await queryClient
    .fetchQuery({ ...recentQuery, staleTime: 0 })
    .catch((): string[] => []);
  return recent.find((id) => ids.includes(id)) ?? ids[0];
}

// createAccessChangeHandler is what runs when the server refuses a request in a
// way that means the user's access changed (D-10). It is plain so it can be
// tested without React: the hook below wires it to the router and the toasts.
export function createAccessChangeHandler(deps: AccessChangeDeps) {
  const { queryClient } = deps;
  const now = deps.now ?? Date.now;
  let lastRun = Number.NEGATIVE_INFINITY;

  return async function onAccessChange(kind: AccessChangeKind, _path: string): Promise<void> {
    // The person asked for something and was told no: say so every time, even
    // when the refetch below is held back.
    if (kind === "refused") deps.notifyRefused();

    const at = now();
    if (at - lastRun < ACCESS_REFETCH_THROTTLE_MS) return;
    lastRun = at;

    const before = queryClient.getQueryData<Me>(meQueryOptions.queryKey);
    setPending(1);
    try {
      let after: Me;
      try {
        // A read already in flight may have been asked before the change, so it is
        // dropped and a new one is made.
        await queryClient.cancelQueries({ queryKey: meQueryOptions.queryKey });
        after = await queryClient.fetchQuery({ ...meQueryOptions, staleTime: 0 });
      } catch {
        // Signed out (the 401 handler takes over) or Bayplan unreachable: there
        // is nothing to move to, and the pages show their own errors.
        return;
      }
      const changed = before !== undefined && accessKey(before) !== accessKey(after);
      const envId = deps.currentEnvId();
      const lost = changed && envId !== undefined && !mayOpen(after, envId);
      if (lost) {
        let target: string | undefined;
        if (!hasNoEnvironmentAccess(after)) {
          target = await nextEnvironment(queryClient).catch(() => undefined);
        }
        deps.moveTo(target);
        deps.notifyChanged();
      } else if (changed && kind === "unseen") {
        deps.notifyChanged();
      }
    } finally {
      setPending(-1);
    }
  };
}

// useAccessRefetch arms the access-change handler while the signed-in layout is
// mounted (D-10). Outside it nothing reacts.
export function useAccessRefetch(): void {
  const t = useT();
  const router = useRouter();
  const queryClient = useQueryClient();
  // The handler reads the latest words and router without being rebuilt, so its
  // throttle window survives a render.
  const say = useRef(t);
  say.current = t;

  useEffect(() => {
    const handler = createAccessChangeHandler({
      queryClient,
      currentEnvId: () => {
        const path = stripBase(router.state.location.pathname);
        return /^\/env\/([^/]+)/.exec(path)?.[1];
      },
      moveTo: (envId) => {
        void router.navigate(
          envId
            ? { to: "/env/$envId", params: { envId }, replace: true }
            : { to: "/", replace: true },
        );
      },
      notifyChanged: () => toastInfo(say.current("noaccess.changed"), SUCCESS_TOAST_MS),
      notifyRefused: () => toastError(say.current("noaccess.refused")),
    });
    setAccessChangeHandler((kind, path) => void handler(kind, path));
    return () => setAccessChangeHandler(null);
  }, [queryClient, router]);
}
