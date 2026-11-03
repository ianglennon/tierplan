// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useIsMutating, useMutation, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { readProblem } from "@/api/problem";
import type { components } from "@/api/schema";
import { useT } from "@/i18n/use-t";
import { toastError, toastSuccess } from "@/lib/toast";

export type UpdateInfo = components["schemas"]["UpdateInfoBody"];
export type UpdateState = UpdateInfo["state"];
export type UpdateReason = UpdateInfo["reason"];
export type CheckUpdatesResult = components["schemas"]["CheckUpdatesBody"];

// CheckUpdatesError is a Check now the server refused or could not answer. The status and the
// problem type tell a standalone host (409 not_a_swarm) or a check that outlasted its minute (504
// check_timed_out) from every other failure; the person gets one generic line either way.
export class CheckUpdatesError extends Error {
  readonly status: number;
  readonly type?: string;
  constructor(status: number, type?: string) {
    super(`POST updates/check failed (${status})`);
    this.name = "CheckUpdatesError";
    this.status = status;
    this.type = type;
  }
}

function checkKey(envId: string) {
  return ["env", envId, "updates", "check"] as const;
}

// useCheckingUpdates says whether a Check now of the environment is out. It reads the mutation
// cache, so it is true in every menu of the page, and a second press waits for the first.
export function useCheckingUpdates(envId: string): boolean {
  return useIsMutating({ mutationKey: checkKey(envId) }) > 0;
}

// useCheckUpdates is the mutation behind Check now: it asks the registries again for the
// environment's images. It is never retried (P2 D-42), since a person pressing the button again
// is the retry. The toasts are part of the mutation, not of the call, because the menu that starts
// it closes and unmounts before the answer arrives. Whatever the answer, every list of the
// environment is read again: a check that timed out still stored what it learned, and each list
// carries the update objects.
export function useCheckUpdates(envId: string) {
  const t = useT();
  const queryClient = useQueryClient();
  return useMutation<CheckUpdatesResult, CheckUpdatesError, void>({
    retry: false,
    mutationKey: checkKey(envId),
    mutationFn: async () => {
      try {
        const res = await api.POST("/api/env/{envId}/updates/check", {
          params: { path: { envId } },
        });
        if (!res.data) {
          const problem = readProblem(res.error, res.response);
          throw new CheckUpdatesError(problem.status, problem.type);
        }
        return res.data;
      } catch (e) {
        // A network failure has no status.
        throw e instanceof CheckUpdatesError ? e : new CheckUpdatesError(0);
      }
    },
    onSuccess: (result) => {
      toastSuccess(
        `${t("updates.check.checked", { count: result.checked })} ${t("updates.check.found", { count: result.updates })}`,
      );
    },
    onError: () => {
      toastError(t("updates.check.failed"));
    },
    onSettled: () => {
      void queryClient.invalidateQueries({ queryKey: ["env", envId] });
    },
  });
}
