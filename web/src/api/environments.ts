// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { queryOptions } from "@tanstack/react-query";

import { api } from "@/api/client";
import { readProblem } from "@/api/problem";
import type { components } from "@/api/schema";

export type EnvironmentItem = components["schemas"]["EnvironmentBody"];
export type EnvironmentSummary = components["schemas"]["EnvironmentSummaryBody"];
export type EnvironmentStatus = components["schemas"]["EnvironmentStatusBody"];
export type HealthState = EnvironmentStatus["state"];

// The dashboard counts refetch on this interval as the polling fallback while
// the events stream is not there (D-57).
export const SUMMARY_REFETCH_MS = 30_000;

// The ["environments"] query: every environment the caller may read. The
// switcher, the home redirect and the setup panel all read it, and the panel
// invalidates it after adding the local host.
export const environmentsQuery = queryOptions({
  queryKey: ["environments"],
  queryFn: async () => {
    const { data } = await api.GET("/api/environments");
    if (!data) throw new Error("GET /api/environments failed");
    return data;
  },
});

// The ["recent-environments"] query: the environments this user opened last,
// newest first, at most 3, from the server so it follows them across devices
// (D-31). Ones that were removed or are no longer readable are already left out.
export const recentQuery = queryOptions({
  queryKey: ["recent-environments"],
  queryFn: async (): Promise<string[]> => {
    const { data } = await api.GET("/api/me/recent-environments");
    if (!data) throw new Error("GET /api/me/recent-environments failed");
    return data.environmentIds ?? [];
  },
});

// recordRecent tells the server this environment was just opened. A failure
// is of no use to the person who opened it, so it never throws.
export async function recordRecent(envId: string): Promise<boolean> {
  try {
    const { response } = await api.PUT("/api/me/recent-environments/{envId}", {
      params: { path: { envId } },
    });
    return response.ok;
  } catch {
    return false;
  }
}

export type EnvironmentEngineFacts = components["schemas"]["EnvironmentProblemEngine"];

// What the environment pages turn a failed environment read into.
export type EnvironmentFailure = "offline" | "unsupported" | "not_found" | "forbidden" | "other";

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

function text(v: unknown): string | undefined {
  return typeof v === "string" && v !== "" ? v : undefined;
}

// EnvironmentProblemError is a failed environment read with the members the
// full-page states need: when the engine stopped answering, why, and which
// engine was found. Daemon-supplied strings are only ever rendered as text
// (T-01-81).
export class EnvironmentProblemError extends Error {
  readonly status: number;
  readonly type?: string;
  readonly since?: string;
  readonly reason?: string;
  readonly engine?: EnvironmentEngineFacts;

  constructor(body: unknown, response?: { status: number }) {
    const base = readProblem(body, response);
    super(`environment request failed with status ${base.status}`);
    this.name = "EnvironmentProblemError";
    this.status = base.status;
    this.type = base.type;
    const rec = isRecord(body) ? body : {};
    this.since = text(rec.since);
    this.reason = text(rec.reason);
    if (isRecord(rec.engine)) {
      this.engine = {
        name: text(rec.engine.name),
        version: text(rec.engine.version),
        apiVersion: text(rec.engine.apiVersion),
      };
    }
  }

  // The problem `type` is what tells an environment that is down from Bayplan
  // itself being unreachable behind a proxy, which also answers 503.
  get failure(): EnvironmentFailure {
    if (this.type === "environment_offline") return "offline";
    if (this.type === "environment_unsupported") return "unsupported";
    if (this.status === 404) return "not_found";
    if (this.status === 403) return "forbidden";
    return "other";
  }
}

// summaryQuery is the dashboard read of one environment. The layout and the
// dashboard page use the same options, so they share one cache entry and one
// request: the staleTime keeps the page from refetching what the layout just
// loaded. A problem answer is the server's verdict, so it is not retried.
export function summaryQuery(envId: string) {
  return queryOptions({
    queryKey: ["env", envId, "summary"],
    queryFn: async (): Promise<EnvironmentSummary> => {
      const { data, error, response } = await api.GET("/api/env/{envId}/summary", {
        params: { path: { envId } },
      });
      if (data) return data;
      throw new EnvironmentProblemError(error, response);
    },
    refetchInterval: SUMMARY_REFETCH_MS,
    staleTime: 10_000,
    retry: (count, err) => !(err instanceof EnvironmentProblemError) && count < 1,
  });
}

// reconnectEnvironment asks the health monitor to probe the engine now. The
// answer is 202 and the result shows up in a later read, so callers poll.
export async function reconnectEnvironment(envId: string): Promise<void> {
  await api.POST("/api/env/{envId}/reconnect", { params: { path: { envId } } });
}
