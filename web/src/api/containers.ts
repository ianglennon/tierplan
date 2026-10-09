// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useMutation, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { readProblem } from "@/api/problem";
import { listKey } from "@/api/resources";
import type { components } from "@/api/schema";

export type ContainerActionResult = components["schemas"]["ContainerActionResult"];
export type ContainerActionRequest = components["schemas"]["ContainerActionRequest"];
export type ContainerRemoveRequest = components["schemas"]["ContainerRemoveRequest"];
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
  // The problem type the server named (confirmation_required, forbidden,
  // agent_policy_denied ...), when it answered with one.
  readonly type?: string;
  constructor(status: number, type?: string) {
    super(`POST containers/actions failed (${status})`);
    this.name = "ActionRequestError";
    this.status = status;
    this.type = type;
  }
}

// The most targets one request takes (the server refuses more with 422).
export const BULK_CHUNK = 25;

// BulkAnswer is what one request answers: the results in the order asked, and
// the bulk ID the server recorded in the audit rows.
export interface BulkAnswer {
  bulkId?: string;
  results: ContainerActionResult[];
}

// The body of a request apart from its targets and its bulk ID, which runBulk
// sets for each chunk.
export type BulkBody =
  | ({ kind: "action" } & Omit<ContainerActionRequest, "targets" | "bulkId">)
  | ({ kind: "remove" } & Omit<ContainerRemoveRequest, "targets" | "bulkId">);

async function post(
  envId: string,
  body: BulkBody,
  targets: ContainerActionTarget[],
  bulkId: string | undefined,
): Promise<BulkAnswer> {
  try {
    const { kind, ...rest } = body;
    const res =
      kind === "remove"
        ? await api.POST("/api/env/{envId}/containers/remove", {
            params: { path: { envId } },
            body: { ...(rest as Omit<ContainerRemoveRequest, "targets">), targets, bulkId },
          })
        : await api.POST("/api/env/{envId}/containers/actions", {
            params: { path: { envId } },
            body: { ...(rest as Omit<ContainerActionRequest, "targets">), targets, bulkId },
          });
    if (!res.data) {
      const problem = readProblem(res.error, res.response);
      throw new ActionRequestError(problem.status, problem.type);
    }
    return { bulkId: res.data.bulkId, results: res.data.results ?? [] };
  } catch (e) {
    // A network failure has no status.
    throw e instanceof ActionRequestError ? e : new ActionRequestError(0);
  }
}

// postContainerAction asks for one action on up to 25 containers and returns
// one result per target, in the order asked. It never retries: a write whose
// answer was lost may have happened (P2 D-42).
export async function postContainerAction(
  envId: string,
  body: ContainerActionRequest,
): Promise<ContainerActionResult[]> {
  const { targets, bulkId, ...rest } = body;
  const answer = await post(envId, { kind: "action", ...rest }, targets ?? [], bulkId);
  return answer.results;
}

// runBulk runs one action (or a remove) on any number of containers. The server
// takes 25 at a time, so a longer list goes out in chunks one after the other
// (the node and overall limits are the server's), and every chunk after the
// first carries the bulk ID the first one was answered with, so all the audit
// rows of one operation share it (D-14). The results are merged in the order
// asked. If the first chunk is refused as a whole, that is thrown; if a later
// one is, its targets are answered as failed and the earlier chunks stand.
export function runBulk(
  envId: string,
  body: BulkBody,
  targets: ContainerActionTarget[],
): Promise<ContainerActionResult[]> {
  return runChunks(envId, body, targets);
}

async function runChunks(
  envId: string,
  body: BulkBody,
  targets: ContainerActionTarget[],
): Promise<ContainerActionResult[]> {
  const results: ContainerActionResult[] = [];
  let bulkId: string | undefined;
  for (let at = 0; at < targets.length; at += BULK_CHUNK) {
    const chunk = targets.slice(at, at + BULK_CHUNK);
    try {
      const answer = await post(envId, body, chunk, bulkId);
      bulkId ??= answer.bulkId;
      results.push(...answer.results);
    } catch (e) {
      if (at === 0) throw e;
      for (const t of chunk) {
        results.push({
          nodeId: t.nodeId,
          id: t.id,
          name: t.id,
          result: "failed",
          code: "action_failed",
        });
      }
    }
  }
  return results;
}

// The refresh that follows every write: the lists and the summary are read
// again at once, and once more SETTLE_RECHECK_MS later in case the Docker event
// that confirms the change is late.
function refreshAfter(queryClient: ReturnType<typeof useQueryClient>, envId: string) {
  const refresh = () => {
    void queryClient.invalidateQueries({ queryKey: listKey(envId, "containers") });
    void queryClient.invalidateQueries({ queryKey: ["env", envId, "summary"] });
  };
  refresh();
  setTimeout(refresh, SETTLE_RECHECK_MS);
}

export interface BulkVariables {
  body: BulkBody;
  targets: ContainerActionTarget[];
}

// useBulkAction is the mutation behind the dialogs and the bulk bar. It does not
// retry, and refreshes the lists whatever the answer.
export function useBulkAction(envId: string) {
  const queryClient = useQueryClient();
  return useMutation<ContainerActionResult[], ActionRequestError, BulkVariables>({
    mutationFn: ({ body, targets }) => runBulk(envId, body, targets),
    retry: false,
    onSettled: () => refreshAfter(queryClient, envId),
  });
}

// useContainerAction is the mutation behind the row menu's immediate actions.
// It does not retry. Whatever the answer, the containers list and the summary
// are read again at once, and once more SETTLE_RECHECK_MS later.
export function useContainerAction(envId: string) {
  const queryClient = useQueryClient();
  return useMutation<ContainerActionResult[], ActionRequestError, ContainerActionRequest>({
    mutationFn: (body) => postContainerAction(envId, body),
    retry: false,
    onSettled: () => refreshAfter(queryClient, envId),
  });
}
