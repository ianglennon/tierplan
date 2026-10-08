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

// What the user holds on an environment: the permissions, the extras an Operator
// assignment carries and the rank of the role (viewer 1, operator 2, admin 3). The
// permissions alone cannot tell a Viewer from an Operator while the registry gives
// them the same list, so the role and extras count too.
interface Holding {
  perms: Set<string>;
  extras: Set<string>;
  rank: number;
}

const RANK: Record<string, number> = { viewer: 1, operator: 2, admin: 3 };

function emptyHolding(): Holding {
  return { perms: new Set(), extras: new Set(), rank: 0 };
}

// What the user has held, as every /api/me answer has told us: each environment ID
// (or "*" for every environment) and what was seen for it.
type Baseline = Map<string, Holding>;

// holding is what /api/me says the user holds under one key of its access map.
function holding(me: Me, key: string): Holding {
  const grant = me.access[key];
  return {
    perms: new Set(me.permissions.environments[key] ?? []),
    extras: new Set(grant?.extras ?? []),
    rank: grant ? (RANK[grant.role] ?? 0) : 0,
  };
}

// merge adds what b holds to a.
function merge(a: Holding, b: Holding): Holding {
  return {
    perms: new Set([...a.perms, ...b.perms]),
    extras: new Set([...a.extras, ...b.extras]),
    rank: Math.max(a.rank, b.rank),
  };
}

// covers is true when what the user holds now has everything in then.
function covers(now: Holding, then: Holding): boolean {
  if (now.rank < then.rank) return false;
  for (const perm of then.perms) if (!now.perms.has(perm)) return false;
  for (const extra of then.extras) if (!now.extras.has(extra)) return false;
  return true;
}

// createAccessChangeHandler is what runs when the server refuses a request in a
// way that means the user's access changed (D-10). It is plain so it can be
// tested without React: the hook below wires it to the router and the toasts.
//
// It decides from a baseline it owns, not from a snapshot of the ["me"] cache
// taken when the refusal is handled. Opening a page mounts components that read
// /api/me again, and the server audits a refusal before it sends it, so the cache
// often already holds the new access when the refusal arrives (G-03-2). The
// baseline is fed by every /api/me answer through a cache subscription and is
// only ever added to until the user is told about a change, so a re-read can
// never hide a loss: lost means held in the baseline and not openable now.
export function createAccessChangeHandler(deps: AccessChangeDeps) {
  const { queryClient } = deps;
  const now = deps.now ?? Date.now;
  const meKey = meQueryOptions.queryKey;
  let lastRun = Number.NEGATIVE_INFINITY;

  const baseline: Baseline = new Map();
  let baselineUser: string | undefined;

  // observe adds what an answer says the user holds. A different user starts the
  // baseline again, so one person's access is never carried into another's
  // session (T-03-123).
  const observe = (me: Me): void => {
    if (baselineUser !== undefined && me.user.id !== baselineUser) baseline.clear();
    baselineUser = me.user.id;
    for (const key of Object.keys(me.access)) {
      baseline.set(key, merge(baseline.get(key) ?? emptyHolding(), holding(me, key)));
    }
  };
  const reset = (me: Me): void => {
    baseline.clear();
    baselineUser = undefined;
    observe(me);
  };

  const seeded = queryClient.getQueryData<Me>(meKey);
  if (seeded) observe(seeded);
  const unsubscribe = queryClient.getQueryCache().subscribe((event) => {
    const key = event.query.queryKey;
    if (key.length !== 1 || key[0] !== meKey[0]) return;
    const data = event.query.state.data as Me | undefined;
    if (data) observe(data);
  });

  // What the baseline holds for an environment, counting access to all of them.
  const heldFor = (envId: string): Holding =>
    merge(baseline.get("*") ?? emptyHolding(), baseline.get(envId) ?? emptyHolding());
  const has = (envId: string): boolean => baseline.has(envId) || baseline.has("*");
  // Whether the user keeps everything the baseline held for an environment.
  const keeps = (me: Me, envId: string): boolean =>
    covers(merge(holding(me, "*"), holding(me, envId)), heldFor(envId));

  async function handle(kind: AccessChangeKind, _path: string): Promise<void> {
    // The person asked for something and was told no: say so every time, even
    // when the refetch below is held back.
    if (kind === "refused") deps.notifyRefused();

    const at = now();
    if (at - lastRun < ACCESS_REFETCH_THROTTLE_MS) return;
    lastRun = at;

    setPending(1);
    try {
      let after: Me;
      try {
        // A read already in flight may have been asked before the change, so it is
        // dropped and a new one is made.
        await queryClient.cancelQueries({ queryKey: meKey });
        after = await queryClient.fetchQuery({ ...meQueryOptions, staleTime: 0 });
      } catch {
        // Signed out (the 401 handler takes over) or Bayplan unreachable: there
        // is nothing to move to, and the pages show their own errors.
        return;
      }
      const envId = deps.currentEnvId();
      let lost = false;
      let narrowed = false;
      if (envId !== undefined) {
        lost = has(envId) && !mayOpen(after, envId);
        narrowed = has(envId) && !lost && !keeps(after, envId);
      } else {
        // Off an environment page there is nothing to move from, but a refusal
        // still deserves the toast when what the user held has shrunk.
        for (const key of baseline.keys()) {
          const open = key === "*" ? after.access["*"] !== undefined : mayOpen(after, key);
          if (!open || !keeps(after, key)) narrowed = true;
        }
      }
      let told = false;
      if (lost) {
        let target: string | undefined;
        if (!hasNoEnvironmentAccess(after)) {
          target = await nextEnvironment(queryClient).catch(() => undefined);
        }
        deps.moveTo(target);
        deps.notifyChanged();
        told = true;
      } else if (narrowed && kind === "unseen") {
        deps.notifyChanged();
        told = true;
      }
      // Once the user knows, the baseline is what they hold now, so the same loss
      // is not reported twice. A run with nothing to say leaves it alone.
      if (told || kind === "refused") reset(after);
    } finally {
      setPending(-1);
    }
  }

  return { handle, dispose: unsubscribe };
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
    const { handle, dispose } = createAccessChangeHandler({
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
    setAccessChangeHandler((kind, path) => void handle(kind, path));
    return () => {
      setAccessChangeHandler(null);
      dispose();
    };
  }, [queryClient, router]);
}
