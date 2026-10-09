// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useMutation, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { readProblem } from "@/api/problem";
import { listKey } from "@/api/resources";
import type { components } from "@/api/schema";

export type ContainerActionResult = components["schemas"]["ContainerActionResult"];
export type ContainerActionRequest = components["schemas"]["ContainerActionRequest"];
export type ContainerActionTarget = components["schemas"]["ContainerActionTarget"];
export type ContainerAction = ContainerActionRequest["action"];
export type ContainerActionCode = NonNullable<ContainerActionResult["code"]>;

// How long after an answer the lists are read once more, in case the Docker
// event that confirms the change is late (UI-SPEC pending state, D-13).
export const SETTLE_RECHECK_MS = 2000;

// ActionRequestError is a request that got no per-container answer at all: the
// server refused it as a whole (permission lost, a bad body, a server fault) or
// could not be reached. A container that failed is not this; it is a result.
export class ActionRequestError extends Error {
  readonly status: number;
  constructor(status: number) {
    super(`POST containers/actions failed (${status})`);
    this.name = "ActionRequestError";
    this.status = status;
  }
}

// postContainerAction asks for one action on up to 25 containers and returns
// one result per target, in the order asked. It never retries: a write whose
// answer was lost may have happened (P2 D-42).
export async function postContainerAction(
  envId: string,
  body: ContainerActionRequest,
): Promise<ContainerActionResult[]> {
  try {
    const res = await api.POST("/api/env/{envId}/containers/actions", {
      params: { path: { envId } },
      body,
    });
    if (!res.data) throw new ActionRequestError(readProblem(res.error, res.response).status);
    return res.data.results ?? [];
  } catch (e) {
    // A network failure has no status.
    throw e instanceof ActionRequestError ? e : new ActionRequestError(0);
  }
}

// useContainerAction is the mutation behind the row menu and, later, the bulk
// bar. It does not retry. Whatever the answer, the containers list and the
// summary are read again at once, and once more SETTLE_RECHECK_MS later.
export function useContainerAction(envId: string) {
  const queryClient = useQueryClient();
  return useMutation<ContainerActionResult[], ActionRequestError, ContainerActionRequest>({
    mutationFn: (body) => postContainerAction(envId, body),
    retry: false,
    onSettled: () => {
      const refresh = () => {
        void queryClient.invalidateQueries({ queryKey: listKey(envId, "containers") });
        void queryClient.invalidateQueries({ queryKey: ["env", envId, "summary"] });
      };
      refresh();
      setTimeout(refresh, SETTLE_RECHECK_MS);
    },
  });
}
