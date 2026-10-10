// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { queryOptions, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { type Problem, readProblem } from "@/api/problem";
import type { components } from "@/api/schema";
import { apiUrl } from "@/lib/base-path";

export type ShellSession = components["schemas"]["ShellSessionBody"];
export type ShellPreference = components["schemas"]["ShellPreferenceBody"];
export type ShellOpenRequest = components["schemas"]["ShellOpenBody"];

// ShellOpenError is a shell the server would not open, with the problem type it
// named (host_read_only, container_not_running, own_container, shell_limit_reached,
// no_shell, agent_policy_denied, stream_busy ...), or status 0 when the server could not
// be reached. For shell_limit_reached, limit is the most shells one person may hold.
export class ShellOpenError extends Error {
  readonly status: number;
  readonly type?: string;
  readonly limit?: number;
  constructor(status: number, type?: string, limit?: number) {
    super(`POST containers/shell failed (${status})`);
    this.name = "ShellOpenError";
    this.status = status;
    this.type = type;
    this.limit = limit;
  }
}

// limitOf reads the cap a shell_limit_reached problem names among its errors.
function limitOf(fields: { location: string; message: string }[]): number | undefined {
  const n = Number(fields.find((f) => f.location === "limit")?.message);
  return Number.isInteger(n) && n > 0 ? n : undefined;
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
      throw new ShellOpenError(
        problem.status,
        problem.type,
        problem.type === "shell_limit_reached" ? limitOf(problem.fields) : undefined,
      );
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

// The longest image reference the server remembers a choice for.
const MAX_PREFERENCE_IMAGE = 512;

// shellPreferenceKey is the cache key of the signed-in user's remembered choice for an
// image, so a successful open can put the new choice there at once.
export function shellPreferenceKey(image: string) {
  return ["shell-preference", image] as const;
}

// useShellPreference reads how the signed-in user last opened a shell in a container of
// this image (D-26), kept on the server so it follows them to another browser. It is
// null when there is none (404), and not asked for without an image. A failure to read it
// is not shown: the launcher then simply starts from its defaults.
export function useShellPreference(image: string | undefined) {
  const ref = image ?? "";
  return useQuery<ShellPreference | null>({
    queryKey: shellPreferenceKey(ref),
    queryFn: async () => {
      const { data, response } = await api.GET("/api/me/shell-preferences", {
        params: { query: { image: ref } },
      });
      if (data) return data;
      if (response.status === 404) return null;
      throw new Error(`GET shell-preferences failed (${response.status})`);
    },
    enabled: ref !== "" && ref.length <= MAX_PREFERENCE_IMAGE,
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

export type ShellSettings = components["schemas"]["ShellSettingsBody"];

// The limits on browser shells (D-29, D-31): how long one may sit unused, and how many
// one person may hold. Read with settings.read, changed with settings.write.
export const shellSettingsQuery = queryOptions({
  queryKey: ["settings", "shells"],
  queryFn: async (): Promise<ShellSettings> => {
    const { data } = await api.GET("/api/settings/shells");
    if (!data) throw new Error("GET /api/settings/shells failed");
    return data;
  },
});

export function useShellSettings() {
  return useQuery(shellSettingsQuery);
}

// ShellSettingsError is a change the server refused, with the problem it answered, or
// status 0 when it could not be reached.
export class ShellSettingsError extends Error {
  readonly problem?: Problem;
  constructor(problem?: Problem) {
    super(`PATCH settings/shells failed (${problem?.status ?? 0})`);
    this.name = "ShellSettingsError";
    this.problem = problem;
  }
}

export function useUpdateShellSettings() {
  const queryClient = useQueryClient();
  return useMutation<ShellSettings, ShellSettingsError, Partial<ShellSettings>>({
    mutationFn: async (body) => {
      try {
        const { data, error, response } = await api.PATCH("/api/settings/shells", { body });
        if (!data) throw new ShellSettingsError(readProblem(error, response));
        return data;
      } catch (e) {
        throw e instanceof ShellSettingsError ? e : new ShellSettingsError();
      }
    },
    retry: false,
    onSuccess: (data) => {
      queryClient.setQueryData(shellSettingsQuery.queryKey, data);
    },
  });
}

export type ShellItem = components["schemas"]["ShellItemBody"];

// ShellEndError is a shell the server would not end, with the problem type it named
// (shell_not_found when it had already closed), or status 0 when it could not be reached.
export class ShellEndError extends Error {
  readonly status: number;
  readonly type?: string;
  constructor(status: number, type?: string) {
    super(`DELETE shells failed (${status})`);
    this.name = "ShellEndError";
    this.status = status;
    this.type = type;
  }
}

// Open shells change as people work and close tabs, so the lists are read again every 15
// seconds and when the window is focused (D-37). There is no stream for them.
export const SHELLS_POLL_MS = 15_000;

// activeShellsKey is the cache key of every open shell on an environment, under the
// environment's own key.
export function activeShellsKey(envId: string) {
  return ["env", envId, "active-shells"] as const;
}

// activeShellsQuery lists every open shell on the environment, whoever opened it. It is
// for someone who holds shells.manage there.
export const activeShellsQuery = (envId: string) =>
  queryOptions({
    queryKey: activeShellsKey(envId),
    queryFn: async (): Promise<ShellItem[]> => {
      const { data } = await api.GET("/api/env/{envId}/active-shells", {
        params: { path: { envId } },
      });
      if (!data) throw new Error("GET /api/env/{envId}/active-shells failed");
      return data.items ?? [];
    },
    refetchInterval: SHELLS_POLL_MS,
    refetchOnWindowFocus: true,
    retry: 1,
  });

async function endShellRequest(
  call: () => Promise<{ error?: unknown; response: Response }>,
): Promise<void> {
  try {
    const { error, response } = await call();
    if (response.ok) return;
    const problem = readProblem(error, response);
    throw new ShellEndError(problem.status, problem.type);
  } catch (e) {
    // A network failure has no status.
    throw e instanceof ShellEndError ? e : new ShellEndError(0);
  }
}

// useEndShell ends someone's shell on the environment at once (shells.manage). A shell
// that had already closed is a ShellEndError of type shell_not_found, and the lists are
// read again either way, so the row that is gone goes.
export function useEndShell(envId: string) {
  const queryClient = useQueryClient();
  return useMutation<void, ShellEndError, { id: string }>({
    mutationFn: ({ id }) =>
      endShellRequest(() =>
        api.DELETE("/api/env/{envId}/active-shells/{shellId}", {
          params: { path: { envId, shellId: id } },
        }),
      ),
    retry: false,
    onSettled: () => queryClient.invalidateQueries({ queryKey: activeShellsKey(envId) }),
  });
}
