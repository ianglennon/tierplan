// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { useEffect, useMemo, useState } from "react";

import { api } from "@/api/client";
import { type ProblemField, readProblem } from "@/api/problem";
import type { components } from "@/api/schema";
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

// A variable of a pasted file as it is sent: the name, the value typed for it and whether it is secret.
export type PastedVar = { name: string; value: string; secret: boolean };

// The original compose file a person pasted, and the values of the variables it uses. Untrusted text that may hold
// secrets: it lives in the page's state and in the request, and nowhere else.
export type PastedFile = { compose: string; vars: PastedVar[] };

// failureOf is the AdoptRequestError of a response that carries no data.
function failureOf(res: { error?: unknown; response: Response }): AdoptRequestError {
  const problem = readProblem(res.error, res.response);
  const detail = (res.error as { detail?: unknown } | undefined)?.detail;
  return new AdoptRequestError(
    problem.status,
    problem.type,
    problem.fields,
    typeof detail === "string" ? detail : "",
  );
}

// useAdoptStack adopts the unmanaged stack of that name, with the fingerprint the page read. The server rebuilds the
// definition itself and sends nothing to the engine; the body carries nothing else, unless the person pasted their
// original compose file: then it also carries that text and its variables, which the server checks again and stores
// as written (the browser never sends a verdict). It is never retried: an adoption whose answer was lost may have been
// stored, and the lists, read again, show whether it was. The stack lists and the pages under them are read again
// whatever the answer. The mutation is dropped as soon as nothing observes it (gcTime 0), so the pasted text does not
// outlive the page in the mutation cache.
export function useAdoptStack(envId: string, name: string) {
  const queryClient = useQueryClient();
  return useMutation<StackDetail, AdoptRequestError, { fingerprint: string; pasted?: PastedFile }>({
    mutationFn: async ({ fingerprint, pasted }) => {
      try {
        const res = await api.POST("/api/env/{envId}/unmanaged-stacks/{name}/adopt", {
          params: { path: { envId, name } },
          body: pasted
            ? { fingerprint, source: "paste", compose: pasted.compose, vars: pasted.vars }
            : { fingerprint, source: "rebuild" },
        });
        if (!res.data) throw failureOf(res);
        return res.data;
      } catch (e) {
        // A network failure has no status.
        throw e instanceof AdoptRequestError ? e : new AdoptRequestError(0);
      }
    },
    retry: false,
    gcTime: 0,
    onSettled: () => invalidateStacks(queryClient, envId),
  });
}

export type AdoptCheck = components["schemas"]["AdoptCheckBody"];

// The query key of an adopt check. It is deliberately not under the stacks key: the events feed refreshes everything
// under that key, and a check that read again by itself would change the fingerprint under a person who is about to
// press Adopt. A check runs on entry and when the person asks.
export function adoptCheckKey(envId: string, name: string) {
  return ["adopt-check", envId, name] as const;
}

// The check of what adopting the stack would change on its first deploy: a read of the engine that changes nothing,
// made with POST because it rebuilds the definition. It runs when the Adopt page opens and again only on "Check
// again" and "Retry check" (refetch); it is never kept for a later visit, so the fingerprint Adopt sends is always from
// a check made while the person was here. A refusal is an AdoptRequestError, like the adoption's own.
export function adoptCheckQuery(envId: string, name: string) {
  return queryOptions({
    queryKey: adoptCheckKey(envId, name),
    queryFn: async (): Promise<AdoptCheck> => {
      try {
        const res = await api.POST("/api/env/{envId}/unmanaged-stacks/{name}/adopt-check", {
          params: { path: { envId, name } },
        });
        if (!res.data) throw failureOf(res);
        return res.data;
      } catch (e) {
        throw e instanceof AdoptRequestError ? e : new AdoptRequestError(0);
      }
    },
    retry: false,
    staleTime: Number.POSITIVE_INFINITY,
    gcTime: 0,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
  });
}

// How long the page waits after the pasted text or a variable changes before it checks the file again.
export const PASTE_CHECK_DELAY_MS = 800;

// The most a pasted file may be, as the server's check judges it.
export const MAX_PASTED_BYTES = 1024 * 1024;

type PastedResult = { input: PastedFile; data?: AdoptCheck; error?: AdoptRequestError };

export interface PastedCheck {
  // The check of the pasted file in its current state, once it has settled; absent while it waits or runs, and when it
  // failed.
  data?: AdoptCheck;
  // Why the check failed, when it did.
  error?: AdoptRequestError;
  // True from a change of the text or of a variable until the check of that change has answered.
  checking: boolean;
  // Checks the file again now, as "Retry check" and "Check again" ask.
  retry: () => void;
}

// usePastedCheck checks a pasted compose file against what runs, 800 ms after the text or a variable last changed. It
// is plain component state and not a query or a mutation: the text may hold secrets, and a query key, a query result
// and a mutation's variables all live in a cache that outlives the page. A change cancels the check in flight. input is
// null when there is nothing to check; it must keep its identity while its content is the same, because a check is
// taken to belong to the input it was made for.
export function usePastedCheck(envId: string, name: string, input: PastedFile | null): PastedCheck {
  const [result, setResult] = useState<PastedResult | null>(null);
  const [tick, setTick] = useState(0);

  // biome-ignore lint/correctness/useExhaustiveDependencies: tick only asks for the check to run again
  useEffect(() => {
    if (!input) {
      setResult(null);
      return;
    }
    const controller = new AbortController();
    const timer = setTimeout(() => {
      void (async () => {
        try {
          const res = await api.POST("/api/env/{envId}/unmanaged-stacks/{name}/adopt-check", {
            params: { path: { envId, name } },
            body: { source: "paste", compose: input.compose, vars: input.vars },
            signal: controller.signal,
          });
          if (controller.signal.aborted) return;
          setResult(res.data ? { input, data: res.data } : { input, error: failureOf(res) });
        } catch {
          // An abort is a newer check taking over; any other failure is a lost connection.
          if (!controller.signal.aborted) setResult({ input, error: new AdoptRequestError(0) });
        }
      })();
    }, PASTE_CHECK_DELAY_MS);
    return () => {
      clearTimeout(timer);
      controller.abort();
    };
  }, [envId, name, input, tick]);

  const settled = input && result?.input === input ? result : undefined;
  return useMemo(
    () => ({
      data: settled?.data,
      error: settled?.error,
      checking: input !== null && !settled,
      retry: () => {
        setResult(null);
        setTick((n) => n + 1);
      },
    }),
    [input, settled],
  );
}
