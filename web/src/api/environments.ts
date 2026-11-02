// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { type QueryClient, queryOptions } from "@tanstack/react-query";

import { api } from "@/api/client";
import { readProblem } from "@/api/problem";
import type { components } from "@/api/schema";

export type EnvironmentItem = components["schemas"]["EnvironmentBody"];
export type EnvironmentSummary = components["schemas"]["EnvironmentSummaryBody"];
export type EnvironmentStatus = components["schemas"]["EnvironmentStatusBody"];
export type HealthState = EnvironmentStatus["state"];
export type InstallInfo = components["schemas"]["InstallBody"];
export type RotateAnswer = components["schemas"]["RotateTokenBody"];
export type UnknownAgentItem = components["schemas"]["UnknownAgentBody"];

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

// The ["unknown-agents"] query: the agents that tried to connect with a certificate
// the environment issued but are not in this TierPlan's database, as after a restore
// from an older backup (D-31). The server keeps the list in memory and ages it out
// 24 hours after an agent stops trying, so it is read again now and then. Only an
// instance admin may read it.
export const UNKNOWN_AGENTS_REFETCH_MS = 60_000;
export const unknownAgentsQuery = queryOptions({
  queryKey: ["unknown-agents"],
  queryFn: async (): Promise<UnknownAgentItem[]> => {
    const { data } = await api.GET("/api/environments/unknown-agents");
    if (!data) throw new Error("GET /api/environments/unknown-agents failed");
    return data.items ?? [];
  },
  refetchInterval: UNKNOWN_AGENTS_REFETCH_MS,
});

// The ["recent-environments"] query: the environments this user opened last,
// newest first, at most 3, from the server so it follows them across devices
// (D-31). Ones that were removed or are no longer readable are already left out.
// It is not retried: the home redirect waits on it, and the switcher's Recent
// group is only a shortcut, so a failure just leaves the group out.
export const recentQuery = queryOptions({
  queryKey: ["recent-environments"],
  retry: false,
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

// The states a problem answer can name (EnvironmentProblem.state): the ones in
// which an environment's own pages cannot be shown.
export type ProblemState = NonNullable<components["schemas"]["EnvironmentProblem"]["state"]>;

const PROBLEM_STATES: readonly ProblemState[] = [
  "offline",
  "unsupported",
  "pending",
  "reconnecting",
  "revoked",
  "cluster_changed",
];

// isProblemState is true for a health state whose environment has no pages to
// show: the full-page states of D-33. online, degraded and unknown are not.
export function isProblemState(state: string | undefined): state is ProblemState {
  return PROBLEM_STATES.some((s) => s === state);
}

// EnvironmentProblemError is a failed environment read with the members the
// full-page states need: the health state, when it began, why, and which
// engine was found. Daemon-supplied strings are only ever rendered as text
// (T-01-81).
export class EnvironmentProblemError extends Error {
  readonly status: number;
  readonly type?: string;
  // The environment's health state as the server rolled it up. Absent from a
  // server that does not send it, and then the failure alone decides.
  readonly state?: ProblemState;
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
    const state = text(rec.state);
    this.state = isProblemState(state) ? state : undefined;
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

  // The problem `type` is what tells an environment that is down from TierPlan
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

// InstallRequestError is a failed create or rotate call. It keeps the status
// and the problem type, which is all the dialog needs to pick its message, and
// never the body: the answer to a success carries the join token, and nothing
// that failed should be kept around either.
export class InstallRequestError extends Error {
  readonly status: number;
  readonly type?: string;

  constructor(body: unknown, response?: { status: number }) {
    const problem = readProblem(body, response);
    super(`install request failed with status ${problem.status}`);
    this.name = "InstallRequestError";
    this.status = problem.status;
    this.type = problem.type;
  }

  // The server has no address to give an agent: the admin has to set the
  // public URL or the agent URL first.
  get publicUrlRequired(): boolean {
    return this.status === 409 && this.type === "public_url_required";
  }

  // Agents would be given a plain ws:// address for another host, which they
  // refuse. The admin fixes the URL in Settings (or, for development only,
  // sets the server's TIERPLAN_AGENT_ALLOW_INSECURE).
  get insecureAgentUrl(): boolean {
    return this.status === 409 && this.type === "insecure_agent_url";
  }
}

export interface CreatedAgentEnvironment {
  id: string;
  name: string;
  install: InstallInfo;
}

// createAgentEnvironment makes an environment for hosts that run the agent,
// with its first join token, in one call (D-23). The answer holds the token and
// is shown once, so it is used through useMutation only and never as a query:
// nothing here may put it in the TanStack cache.
export async function createAgentEnvironment(name?: string): Promise<CreatedAgentEnvironment> {
  const { data, error, response } = await api.POST("/api/environments", {
    body: name ? { kind: "agent", name } : { kind: "agent" },
  });
  if (!data?.install) throw new InstallRequestError(error, response);
  return { id: data.id, name: data.name, install: data.install };
}

// rotateToken mints the next join token for an environment and retires the
// current one. Like createAgentEnvironment, it is a mutation and never cached.
export async function rotateToken(envId: string): Promise<RotateAnswer> {
  const { data, error, response } = await api.POST("/api/environments/{envId}/token", {
    params: { path: { envId } },
  });
  if (!data) throw new InstallRequestError(error, response);
  return data;
}

// AdoptClusterError is a failed adopt. It keeps the status and the problem type,
// which is what the dialog picks its sentence from.
export class AdoptClusterError extends Error {
  readonly status: number;
  readonly type?: string;

  constructor(body: unknown, response?: { status: number }) {
    const problem = readProblem(body, response);
    super(`adopt failed with status ${problem.status}`);
    this.name = "AdoptClusterError";
    this.status = problem.status;
    this.type = problem.type;
  }

  // Another admin adopted it first, or the change is gone.
  get noChange(): boolean {
    return this.status === 409 && this.type === "no_cluster_change";
  }

  // Another environment manages the new cluster by now.
  get inUse(): boolean {
    return this.status === 409 && this.type === "cluster_in_use";
  }
}

// adoptCluster binds a re-initialised swarm's environment to the cluster its
// managers now report (D-14). It is audited on the server. The caller reads the
// ["environments"] list again afterwards, which is what takes the environment out
// of cluster_changed on every page.
export async function adoptCluster(envId: string): Promise<void> {
  const { error, response } = await api.POST("/api/env/{envId}/adopt-cluster", {
    params: { path: { envId } },
  });
  if (!response.ok) throw new AdoptClusterError(error, response);
}

// FoldLocalError is a failed fold. It keeps the status and the problem type, which is
// what the dialog picks its sentence from.
export class FoldLocalError extends Error {
  readonly status: number;
  readonly type?: string;

  constructor(body: unknown, response?: { status: number }) {
    const problem = readProblem(body, response);
    super(`fold failed with status ${problem.status}`);
    this.name = "FoldLocalError";
    this.status = problem.status;
    this.type = problem.type;
  }

  // The host is no longer a manager of that swarm, or another admin folded it first.
  get notOffered(): boolean {
    return this.status === 409 && this.type === "fold_not_offered";
  }
}

// foldLocal folds this TierPlan host into the swarm environment envId (D-48): the swarm
// reaches the node through the Docker socket and the separate local environment is
// retired. It is audited on the server. The caller reads the lists again afterwards
// with refreshAfterFold.
export async function foldLocal(envId: string): Promise<void> {
  const { error, response } = await api.POST("/api/environments/{envId}/fold-local", {
    params: { path: { envId } },
  });
  if (!response.ok) throw new FoldLocalError(error, response);
}

// refreshAfterFold reads again what a fold changes: the environments list (the local
// environment is gone and nothing offers a fold any more) and the swarm's node list,
// which now has a Local socket row. A fold that was refused as no longer offered
// reads the environments too, since what is offered has changed.
export function refreshAfterFold(queryClient: QueryClient, envId: string): Promise<unknown> {
  return Promise.all([
    queryClient.invalidateQueries({ queryKey: environmentsQuery.queryKey }),
    queryClient.invalidateQueries({ queryKey: ["agents", envId] }),
  ]);
}
