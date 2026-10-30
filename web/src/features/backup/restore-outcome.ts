// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { apiUrl } from "@/lib/base-path";

// What the browser can tell about a restore once Coaming has gone down and come
// back: "restored" (the session it had is refused, so the backup's state is
// running), "rolled_back" (the same session still works, so the restored state
// could not start and the snapshot is running again) or "timeout" (nothing
// answered in time, so nothing can be said).
export type RestoreOutcome = "restored" | "rolled_back" | "timeout";

export interface RestoreOutcomeDeps {
  // Resolves when Coaming answers, rejects when it cannot be reached or is not
  // serving yet (a proxy's 502 counts as not serving).
  fetchVersion: () => Promise<void>;
  // The HTTP status of the call that says whether the caller's session is still
  // good: 401 means refused, 200 means accepted. Rejects when unreachable.
  fetchMe: () => Promise<number>;
  now?: () => number;
  sleep?: (ms: number) => Promise<void>;
  intervalMs?: number;
  timeoutMs?: number;
}

export const POLL_INTERVAL_MS = 1000;
export const POLL_TIMEOUT_MS = 120_000;

// waitForRestoreOutcome polls every second, for up to two minutes, until Coaming
// answers again and the session check gives an answer that can be trusted. A 401
// can only come from the restored state (the session is not in the backup). A 200
// counts only after a failed poll has been seen: before that the answer may come
// from the server that has not stopped yet, which would read as a rollback. A
// status in between (a 502 or 503 while Coaming starts) is not an answer.
export async function waitForRestoreOutcome(deps: RestoreOutcomeDeps): Promise<RestoreOutcome> {
  const now = deps.now ?? Date.now;
  const sleep = deps.sleep ?? ((ms: number) => new Promise<void>((r) => setTimeout(r, ms)));
  const interval = deps.intervalMs ?? POLL_INTERVAL_MS;
  const timeout = deps.timeoutMs ?? POLL_TIMEOUT_MS;
  const started = now();
  let sawDown = false;
  while (now() - started < timeout) {
    try {
      await deps.fetchVersion();
      const status = await deps.fetchMe();
      if (status === 401) return "restored";
      if (status === 200 && sawDown) return "rolled_back";
    } catch {
      sawDown = true;
    }
    await sleep(interval);
  }
  return "timeout";
}

// Each probe gives up after three seconds, so a connection the restart left
// hanging cannot hold the poll.
const PROBE_TIMEOUT_MS = 3000;

function probe(path: string): Promise<Response> {
  return globalThis.fetch(apiUrl(path), {
    cache: "no-store",
    credentials: "same-origin",
    signal: AbortSignal.timeout(PROBE_TIMEOUT_MS),
  });
}

async function reachable(): Promise<void> {
  const response = await probe("/api/version");
  if (!response.ok) throw new Error(`GET /api/version answered ${response.status}`);
}

// sessionProbes are the calls for the Settings page: the version, then /api/me.
// They go around the typed client on purpose, so no handler reacts to the 401.
export function sessionProbes(): Pick<RestoreOutcomeDeps, "fetchVersion" | "fetchMe"> {
  return {
    fetchVersion: reachable,
    fetchMe: async () => (await probe("/api/me")).status,
  };
}

// setupProbes are the calls for the setup wizard, which has no session: the
// instance no longer asking for setup stands for a refused session (401), and
// one that still asks for it stands for an accepted one (200).
export function setupProbes(): Pick<RestoreOutcomeDeps, "fetchVersion" | "fetchMe"> {
  return {
    fetchVersion: reachable,
    fetchMe: async () => {
      const response = await probe("/api/instance");
      if (!response.ok) return response.status;
      const body: unknown = await response.json();
      const needsSetup =
        typeof body === "object" && body !== null && "needsSetup" in body && body.needsSetup;
      return needsSetup ? 200 : 401;
    },
  };
}
