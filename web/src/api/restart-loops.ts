// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { summaryQuery } from "@/api/environments";
import { readProblem } from "@/api/problem";
import { listKey } from "@/api/resources";
import type { components } from "@/api/schema";
import { servicesKey } from "@/api/services";
import { ENV_WARNINGS_KEY } from "@/api/warnings";
import { useT } from "@/i18n/use-t";
import { toastError, toastSuccess } from "@/lib/toast";

export type RestartLoopSettings = components["schemas"]["RestartLoopSettingsBody"];

// The bounds of the per-environment setting, as the server and the database hold them
// (UI-SPEC "Restart-loop setting bounds").
export const FAILURES_MIN = 2;
export const FAILURES_MAX = 50;
export const WINDOW_MIN = 1;
export const WINDOW_MAX = 120;

// RestartLoopRequestError is a mute or a settings save the server refused or never answered. The
// status is 0 when the server could not be reached; the type is the problem type it named.
export class RestartLoopRequestError extends Error {
  readonly status: number;
  readonly type?: string;
  constructor(status: number, type?: string) {
    super(`restart-loop request failed (${status})`);
    this.name = "RestartLoopRequestError";
    this.status = status;
    this.type = type;
  }
}

export function restartLoopSettingsKey(envId: string) {
  return ["env", envId, "restart-loop-settings"] as const;
}

// The environment's threshold and Docker's history limit. For an Env Admin only: the card does
// not read it for anyone else.
export function restartLoopSettingsQuery(envId: string) {
  return queryOptions({
    queryKey: restartLoopSettingsKey(envId),
    queryFn: async (): Promise<RestartLoopSettings> => {
      const res = await api.GET("/api/env/{envId}/restart-loop-settings", {
        params: { path: { envId } },
      });
      if (!res.data) {
        const problem = readProblem(res.error, res.response);
        throw new RestartLoopRequestError(problem.status, problem.type);
      }
      return res.data;
    },
  });
}

// useSaveRestartLoopSettings is the mutation behind the card's Save settings. It does not retry.
export function useSaveRestartLoopSettings(envId: string) {
  const queryClient = useQueryClient();
  return useMutation<
    RestartLoopSettings,
    RestartLoopRequestError,
    { failures: number; windowMinutes: number }
  >({
    retry: false,
    mutationFn: async (body) => {
      try {
        const res = await api.PUT("/api/env/{envId}/restart-loop-settings", {
          params: { path: { envId } },
          body,
        });
        if (!res.data) {
          const problem = readProblem(res.error, res.response);
          throw new RestartLoopRequestError(problem.status, problem.type);
        }
        return res.data;
      } catch (e) {
        // A network failure has no status.
        throw e instanceof RestartLoopRequestError ? e : new RestartLoopRequestError(0);
      }
    },
    onSuccess: (saved) => {
      queryClient.setQueryData(restartLoopSettingsKey(envId), saved);
    },
  });
}

export interface MuteRequest {
  serviceId: string;
  // The name, for the toast: the person knows the service by it.
  name: string;
  // True to mute, false to unmute.
  muted: boolean;
}

// useSetRestartLoopMute mutes or unmutes the restart-loop alerts of one service. The toasts are
// part of the mutation, not of the call, because the menu that starts it closes and unmounts
// before the answer arrives. It reads everything the mute changes again: the services (the badge,
// the detail), the bell and the dashboard tile.
export function useSetRestartLoopMute(envId: string) {
  const t = useT();
  const queryClient = useQueryClient();
  return useMutation<unknown, RestartLoopRequestError, MuteRequest>({
    retry: false,
    mutationFn: async ({ serviceId, muted }) => {
      try {
        const params = { params: { path: { envId, serviceId } } };
        const res = muted
          ? await api.PUT("/api/env/{envId}/services/{serviceId}/restart-loop-mute", params)
          : await api.DELETE("/api/env/{envId}/services/{serviceId}/restart-loop-mute", params);
        if (!res.data) {
          const problem = readProblem(res.error, res.response);
          throw new RestartLoopRequestError(problem.status, problem.type);
        }
        return res.data;
      } catch (e) {
        throw e instanceof RestartLoopRequestError ? e : new RestartLoopRequestError(0);
      }
    },
    onSuccess: (_data, { name, muted }) => {
      toastSuccess(t(muted ? "restartLoop.mute.done" : "restartLoop.mute.undone", { name }));
    },
    onError: (_error, { name }) => {
      toastError(t("restartLoop.mute.failed", { name }));
    },
    onSettled: () => {
      void queryClient.invalidateQueries({ queryKey: servicesKey(envId) });
      void queryClient.invalidateQueries({ queryKey: ENV_WARNINGS_KEY });
      void queryClient.invalidateQueries({ queryKey: summaryQuery(envId).queryKey });
    },
  });
}

export interface ContainerMuteRequest {
  // The node the container is on, as its row of the containers list names it.
  nodeId: string;
  containerId: string;
  // The name, for the toast: the person knows the container by it.
  name: string;
  // True to mute, false to unmute.
  muted: boolean;
}

// useSetContainerRestartLoopMute mutes or unmutes the restart-loop alerts of one standalone
// container (RES-13, D-16). The server keys the mute by node and name. Like the service's, the
// toasts belong to the mutation because the menu closes before the answer arrives, and it reads
// again everything the mute changes: the containers list and pages, the bell and the summary.
export function useSetContainerRestartLoopMute(envId: string) {
  const t = useT();
  const queryClient = useQueryClient();
  return useMutation<unknown, RestartLoopRequestError, ContainerMuteRequest>({
    retry: false,
    mutationFn: async ({ nodeId, containerId, muted }) => {
      try {
        const params = { params: { path: { envId, containerId }, query: { nodeId } } };
        const res = muted
          ? await api.PUT("/api/env/{envId}/containers/{containerId}/restart-loop-mute", params)
          : await api.DELETE("/api/env/{envId}/containers/{containerId}/restart-loop-mute", params);
        if (!res.data) {
          const problem = readProblem(res.error, res.response);
          throw new RestartLoopRequestError(problem.status, problem.type);
        }
        return res.data;
      } catch (e) {
        throw e instanceof RestartLoopRequestError ? e : new RestartLoopRequestError(0);
      }
    },
    onSuccess: (_data, { name, muted }) => {
      toastSuccess(t(muted ? "restartLoop.mute.done" : "restartLoop.mute.undone", { name }));
    },
    onError: (_error, { name }) => {
      toastError(t("restartLoop.mute.failed", { name }));
    },
    onSettled: () => {
      // The detail pages share the list's key prefix.
      void queryClient.invalidateQueries({ queryKey: listKey(envId, "containers") });
      void queryClient.invalidateQueries({ queryKey: ENV_WARNINGS_KEY });
    },
  });
}
