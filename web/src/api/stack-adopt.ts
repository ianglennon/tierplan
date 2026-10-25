// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useMutation, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { type ProblemField, readProblem } from "@/api/problem";
import { invalidateStacks, type StackDetail } from "@/api/stacks";

// AdoptRequestError is an adoption the server refused or that never got an answer. status is 0 when the server could
// not be reached; type is the problem type it named (adopt_stale, stack_already_managed, stack_not_adoptable,
// operation_locked, rebuild_failed, unmanaged_stack_not_found); fields hold what the problem names (stackId of the
// stack that holds the name, stack, job and started of an operation that holds the environment); detail is the
// server's own sentence, which for rebuild_failed names why the stack could not be written as a compose file.
export class AdoptRequestError extends Error {
  readonly status: number;
  readonly type?: string;
  readonly fields: ProblemField[];
  readonly detail: string;
  constructor(status: number, type?: string, fields: ProblemField[] = [], detail = "") {
    super(`POST adopt failed (${status})`);
    this.name = "AdoptRequestError";
    this.status = status;
    this.type = type;
    this.fields = fields;
    this.detail = detail;
  }

  // fieldOf is what the problem named at a location, empty when it named none.
  fieldOf(location: string): string {
    return this.fields.find((f) => f.location === location)?.message ?? "";
  }
}

// useAdoptStack adopts the unmanaged stack of that name, with the fingerprint the page read. The server rebuilds the
// definition itself and sends nothing to the engine; the body carries nothing else. It is never retried: an adoption
// whose answer was lost may have been stored, and the lists, read again, show whether it was. The stack lists and the
// pages under them are read again whatever the answer.
export function useAdoptStack(envId: string, name: string) {
  const queryClient = useQueryClient();
  return useMutation<StackDetail, AdoptRequestError, { fingerprint: string }>({
    mutationFn: async ({ fingerprint }) => {
      try {
        const res = await api.POST("/api/env/{envId}/unmanaged-stacks/{name}/adopt", {
          params: { path: { envId, name } },
          body: { fingerprint },
        });
        if (!res.data) {
          const problem = readProblem(res.error, res.response);
          const detail = (res.error as { detail?: unknown } | undefined)?.detail;
          throw new AdoptRequestError(
            problem.status,
            problem.type,
            problem.fields,
            typeof detail === "string" ? detail : "",
          );
        }
        return res.data;
      } catch (e) {
        // A network failure has no status.
        throw e instanceof AdoptRequestError ? e : new AdoptRequestError(0);
      }
    },
    retry: false,
    onSettled: () => invalidateStacks(queryClient, envId),
  });
}
