// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useMutation } from "@tanstack/react-query";

import { api } from "@/api/client";
import { readProblem } from "@/api/problem";
import type { components } from "@/api/schema";
import { apiUrl } from "@/lib/base-path";

export type ShellSession = components["schemas"]["ShellSessionBody"];
export type ShellOpenRequest = components["schemas"]["ShellOpenBody"];

// ShellOpenError is a shell the server would not open, with the problem type it
// named (host_read_only, container_not_running, no_shell, stream_busy ...), or
// status 0 when the server could not be reached.
export class ShellOpenError extends Error {
  readonly status: number;
  readonly type?: string;
  constructor(status: number, type?: string) {
    super(`POST containers/shell failed (${status})`);
    this.name = "ShellOpenError";
    this.status = status;
    this.type = type;
  }
}

export interface OpenShellVariables {
  envId: string;
  containerId: string;
  body: ShellOpenRequest;
}

// openShell asks the server to start a shell in the container: it creates the exec
// and a pending session the browser then attaches to over a WebSocket. It never
// retries, since a second request would start a second shell.
export async function openShell({
  envId,
  containerId,
  body,
}: OpenShellVariables): Promise<ShellSession> {
  try {
    const res = await api.POST("/api/env/{envId}/containers/{containerId}/shell", {
      params: { path: { envId, containerId } },
      body,
    });
    if (!res.data) {
      const problem = readProblem(res.error, res.response);
      throw new ShellOpenError(problem.status, problem.type);
    }
    return res.data;
  } catch (e) {
    // A network failure has no status.
    throw e instanceof ShellOpenError ? e : new ShellOpenError(0);
  }
}

export function useOpenShell() {
  return useMutation<ShellSession, ShellOpenError, OpenShellVariables>({
    mutationFn: openShell,
    retry: false,
  });
}

// shellSocketUrl is the WebSocket of a pending session: the page's own origin and
// base path with the scheme turned into ws or wss.
export function shellSocketUrl(envId: string, sessionId: string): string {
  const url = new URL(
    apiUrl(
      `/api/env/${encodeURIComponent(envId)}/shell-sessions/${encodeURIComponent(sessionId)}/ws`,
    ),
    window.location.href,
  );
  url.protocol = url.protocol === "https:" ? "wss:" : "ws:";
  return url.toString();
}
