// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { type QueryClient, queryOptions } from "@tanstack/react-query";

import { api } from "@/api/client";
import { environmentsQuery } from "@/api/environments";
import { readProblem } from "@/api/problem";
import type { components } from "@/api/schema";

export type AgentItem = components["schemas"]["AgentBody"];
export type TokenStatus = components["schemas"]["TokenStatusBody"];
// The commands that take the agent off a host by hand (D-30).
export type ManualCleanup = components["schemas"]["InstallCleanupBody"];

// AgentList is the node list with the manual cleanup commands the same read carries.
export interface AgentList {
  items: AgentItem[];
  manualCleanup: ManualCleanup;
}

// How often the node list is read again while an agent is updating. The update
// state is held in memory on the server and is not on the environments stream,
// so the list is polled until nothing is updating.
export const UPDATE_POLL_MS = 3000;

export const isUpdating = (a: AgentItem) => a.updateState?.state === "updating";

// The ["agents", envId] query: every enrolled node's agent, problems first.
// The server's own order is kept; the table sorts again so a row that changed
// from a stream event lands in the right place without waiting for a read.
export function agentsQuery(envId: string) {
  return queryOptions({
    queryKey: ["agents", envId],
    queryFn: async (): Promise<AgentList> => {
      const { data } = await api.GET("/api/env/{envId}/agents", {
        params: { path: { envId } },
      });
      if (!data) throw new Error(`GET /api/env/${envId}/agents failed`);
      return {
        items: data.items ?? [],
        manualCleanup: data.manualCleanup ?? { swarm: "", standalone: "" },
      };
    },
    refetchInterval: (query) => (query.state.data?.items.some(isUpdating) ? UPDATE_POLL_MS : false),
  });
}

// The ["env-token", envId] query: which join token the environment has. Never
// the token itself, which the server keeps only as a hash.
export function tokenStatusQuery(envId: string) {
  return queryOptions({
    queryKey: ["env-token", envId],
    queryFn: async (): Promise<TokenStatus> => {
      const { data } = await api.GET("/api/environments/{envId}/token", {
        params: { path: { envId } },
      });
      if (!data) throw new Error(`GET /api/environments/${envId}/token failed`);
      return data;
    },
  });
}

// AgentActionError is a failed revoke, unblock or disable. It keeps the status
// and the problem type, which is what the dialogs pick their message from.
export class AgentActionError extends Error {
  readonly status: number;
  readonly type?: string;

  constructor(body: unknown, response?: { status: number }) {
    const problem = readProblem(body, response);
    super(`agent request failed with status ${problem.status}`);
    this.name = "AgentActionError";
    this.status = problem.status;
    this.type = problem.type;
  }

  // Someone else revoked the node first, or this admin did in another tab.
  get alreadyRevoked(): boolean {
    return this.status === 409 && this.type === "agent_already_revoked";
  }

  // The node's host is not blocked, so there is nothing to unblock.
  get notBlocked(): boolean {
    return this.status === 409 && this.type === "not_blocked";
  }
}

// revokeAgent disconnects a node's agent and blocks its host (D-08). The server
// answers 204. The reason is optional and kept in the audit log.
export async function revokeAgent(envId: string, agentId: string, reason?: string): Promise<void> {
  const { error, response } = await api.POST(
    "/api/environments/{envId}/agents/{agentId}/revoke",
    reason
      ? { params: { path: { envId, agentId } }, body: { reason } }
      : { params: { path: { envId, agentId } } },
  );
  if (!response.ok) throw new AgentActionError(error, response);
}

// unblockAgent lets a revoked node's host enrol again, as a new agent (D-09).
export async function unblockAgent(envId: string, agentId: string): Promise<void> {
  const { error, response } = await api.POST("/api/environments/{envId}/agents/{agentId}/unblock", {
    params: { path: { envId, agentId } },
  });
  if (!response.ok) throw new AgentActionError(error, response);
}

// disableToken stops the environment's join token enrolling anybody (D-02).
// Agents that are enrolled already keep working. Disabling it twice is fine.
export async function disableToken(envId: string): Promise<void> {
  const { error, response } = await api.DELETE("/api/environments/{envId}/token", {
    params: { path: { envId } },
  });
  if (!response.ok) throw new AgentActionError(error, response);
}

// UpdateActionError is a failed update request. The 409 problems each have a
// sentence of their own: the agent is offline, no manager is connected, the host
// refuses self-update, the agent is up to date, an update is already running.
export class UpdateActionError extends Error {
  readonly status: number;
  readonly type?: string;

  constructor(body: unknown, response?: { status: number }) {
    const problem = readProblem(body, response);
    super(`update request failed with status ${problem.status}`);
    this.name = "UpdateActionError";
    this.status = problem.status;
    this.type = problem.type;
  }
}

// updateAgent starts an update of one agent. In a swarm the agent updates its own
// service, which rolls every node, so this is the same as updating all of them.
// The server answers 202: the update is started, not finished.
export async function updateAgent(envId: string, agentId: string): Promise<void> {
  const { error, response } = await api.POST("/api/environments/{envId}/agents/{agentId}/update", {
    params: { path: { envId, agentId } },
  });
  if (!response.ok) throw new UpdateActionError(error, response);
}

// updateAllAgents starts the updates every agent that is behind needs, and says
// how many it sent: one for a swarm, one per agent otherwise, 0 for none needed.
export async function updateAllAgents(envId: string): Promise<number> {
  const { data, error, response } = await api.POST("/api/environments/{envId}/update-agents", {
    params: { path: { envId } },
  });
  if (!response.ok) throw new UpdateActionError(error, response);
  return data?.started ?? 0;
}

// setAutoUpdate switches the environment's automatic agent updates (D-28).
export async function setAutoUpdate(envId: string, autoUpdate: boolean): Promise<void> {
  const { error, response } = await api.PATCH("/api/env/{envId}", {
    params: { path: { envId } },
    body: { autoUpdate },
  });
  if (!response.ok) throw new UpdateActionError(error, response);
}

// removeEnvironment removes an environment, with its agents uninstalled first from
// the hosts that are online when uninstallAgents is set (D-30). The server waits up
// to 30 seconds for the agents, then revokes and deletes whatever they answered, so
// the request can take that long. A 404 means it is already gone, which is what was
// asked for.
export async function removeEnvironment(envId: string, uninstallAgents: boolean): Promise<void> {
  const { response } = await api.DELETE("/api/environments/{envId}", {
    params: { path: { envId }, query: { uninstallAgents } },
  });
  if (response.ok || response.status === 404) return;
  throw new Error(`DELETE /api/environments/${envId} failed with ${response.status}`);
}

// refreshAfterAgentUpdate is what starting an update invalidates: the node list,
// whose rows now say updating, and the environment list, whose update badge and
// agent counts follow.
export async function refreshAfterAgentUpdate(queryClient: QueryClient, envId: string) {
  await Promise.all([
    queryClient.invalidateQueries({ queryKey: ["agents", envId] }),
    queryClient.invalidateQueries({ queryKey: environmentsQuery.queryKey }),
  ]);
}

// refreshAfterAgentChange is what a revoke, an unblock or a token change
// invalidates: the node table, the environment list (the rollup health and the
// agent counts change with it) and the token status.
export async function refreshAfterAgentChange(queryClient: QueryClient, envId: string) {
  await Promise.all([
    queryClient.invalidateQueries({ queryKey: ["agents", envId] }),
    queryClient.invalidateQueries({ queryKey: environmentsQuery.queryKey }),
    queryClient.invalidateQueries({ queryKey: ["env-token", envId] }),
  ]);
}
