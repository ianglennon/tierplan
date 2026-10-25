// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { queryOptions } from "@tanstack/react-query";

import { api } from "@/api/client";
import { readProblem } from "@/api/problem";
import { LIST_REFETCH_MS } from "@/api/resources";
import type { components } from "@/api/schema";
import {
  STACK_RESOURCES_POLL_MS,
  STACK_SERVICES_POLL_MS,
  type StackResources,
  StacksLoadError,
  stacksKey,
} from "@/api/stacks";

export type UnmanagedStack = components["schemas"]["UnmanagedStackBody"];

// The problem type of the 404 for a name no labelled service or container carries, or one a stack of Bayplan's
// holds now.
const UNMANAGED_GONE = "unmanaged_stack_not_found";

// UnmanagedLoadError is a read of an unmanaged stack the server refused. When a stack of Bayplan's holds the
// name now, the answer names its ID, so the page can offer it.
export class UnmanagedLoadError extends StacksLoadError {
  readonly managedStackId?: string;
  constructor(status: number, type?: string, managedStackId?: string) {
    super(status, type);
    this.name = "UnmanagedLoadError";
    this.managedStackId = managedStackId;
  }
}

// isUnmanagedGone says whether the error is the server answering that no unmanaged stack has this name on the
// environment: it was removed outside Bayplan, or it is managed now.
export function isUnmanagedGone(error: unknown): boolean {
  return error instanceof StacksLoadError && error.status === 404 && error.type === UNMANAGED_GONE;
}

// managedStackIdOf is the ID of the stack of Bayplan's that holds the name now, when the error says one does.
export function managedStackIdOf(error: unknown): string | undefined {
  return error instanceof UnmanagedLoadError ? error.managedStackId : undefined;
}

// The query key of an unmanaged stack's page. It extends the environment's stacks key, so the events feed that
// refreshes the stacks list, and every change that refreshes it, refresh the page too.
export function unmanagedStackKey(envId: string, name: string) {
  return [...stacksKey(envId), "unmanaged", name] as const;
}

// One stack Bayplan did not make, found by its label and read now. A stack that is gone is not retried. A swarm
// polls every 5 s while the page is visible and a standalone host at the lists' pace, as a managed stack's
// overview does; a read that fails keeps the last rows, so the page can say so and still show what it had.
export function unmanagedStackQuery(envId: string, name: string) {
  return queryOptions({
    queryKey: unmanagedStackKey(envId, name),
    queryFn: async (): Promise<UnmanagedStack> => {
      const res = await api.GET("/api/env/{envId}/unmanaged-stacks/{name}", {
        params: { path: { envId, name } },
      });
      if (!res.data) {
        const problem = readProblem(res.error, res.response);
        throw new UnmanagedLoadError(
          problem.status,
          problem.type,
          problem.fields.find((f) => f.location === "managedStackId")?.message,
        );
      }
      return res.data;
    },
    retry: (count, error) => !isUnmanagedGone(error) && count < 3,
    refetchInterval: (query) => {
      if (document.visibilityState !== "visible") return false;
      return query.state.data?.target === "swarm" ? STACK_SERVICES_POLL_MS : LIST_REFETCH_MS;
    },
  });
}

// The query key of what an unmanaged stack uses. It extends the stack's key, so a refresh of the stack refreshes it.
export function unmanagedStackResourcesKey(envId: string, name: string) {
  return [...unmanagedStackKey(envId, name), "resources"] as const;
}

// What an unmanaged stack's services use, one card for each kind, each with its own error, with the ownership of
// each previewing what adopting would own. It polls on a swarm while the page is visible, like a managed stack's.
export function unmanagedStackResourcesQuery(
  envId: string,
  name: string,
  o: { target: UnmanagedStack["target"] },
) {
  return queryOptions({
    queryKey: unmanagedStackResourcesKey(envId, name),
    queryFn: async (): Promise<StackResources> => {
      const res = await api.GET("/api/env/{envId}/unmanaged-stacks/{name}/resources", {
        params: { path: { envId, name } },
      });
      if (!res.data) {
        const problem = readProblem(res.error, res.response);
        throw new UnmanagedLoadError(
          problem.status,
          problem.type,
          problem.fields.find((f) => f.location === "managedStackId")?.message,
        );
      }
      return res.data;
    },
    retry: (count, error) => !isUnmanagedGone(error) && count < 1,
    refetchInterval: () => {
      if (document.visibilityState !== "visible") return false;
      return o.target === "swarm" ? STACK_RESOURCES_POLL_MS : LIST_REFETCH_MS;
    },
  });
}
