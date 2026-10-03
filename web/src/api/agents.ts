// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { type QueryClient, queryOptions } from "@tanstack/react-query";

import { api } from "@/api/client";
import { environmentsQuery } from "@/api/environments";
import { readProblem } from "@/api/problem";
import type { components } from "@/api/schema";

export type AgentItem = components["schemas"]["AgentBody"];
export type TokenStatus = components["schemas"]["TokenStatusBody"];

// The ["agents", envId] query: every enrolled node's agent, problems first.
// The server's own order is kept; the table sorts again so a row that changed
// from a stream event lands in the right place without waiting for a read.
export function agentsQuery(envId: string) {
  return queryOptions({
    queryKey: ["agents", envId],
    queryFn: async (): Promise<AgentItem[]> => {
      const { data } = await api.GET("/api/env/{envId}/agents", {
        params: { path: { envId } },
      });
      if (!data) throw new Error(`GET /api/env/${envId}/agents failed`);
      return data.items ?? [];
    },
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
