// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import {
  keepPreviousData,
  queryOptions,
  useMutation,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";

import { api } from "@/api/client";
import { type ProblemField, readProblem } from "@/api/problem";
import { LIST_REFETCH_MS } from "@/api/resources";
import type { components } from "@/api/schema";
import type { StackLock } from "@/api/stacks";

export type JanitorPreview = components["schemas"]["JanitorPreviewBody"];
export type JanitorItem = components["schemas"]["JanitorItemBody"];
export type JanitorItemRef = components["schemas"]["JanitorItemRef"];
export type JanitorReason = components["schemas"]["JanitorReasonBody"];
export type JanitorSummary = components["schemas"]["JanitorSummaryBody"];
export type JanitorRun = components["schemas"]["JanitorRunBody"];
export type JanitorRunItem = components["schemas"]["JanitorRunItemBody"];
export type JanitorRunStarted = components["schemas"]["JanitorRunStartedBody"];
export type JanitorEvidence = components["schemas"]["JanitorEvidenceBody"];
export type JanitorStatus = components["schemas"]["JanitorStatusBody"];
export type JanitorRunSummary = components["schemas"]["JanitorRunSummaryBody"];
export type JanitorRunList = components["schemas"]["JanitorRunListBody"];
export type JanitorJobSummary = components["schemas"]["JanitorJobSummary"];

// The frames of a run's stream (GET .../janitor/runs/{runId}/stream). The server sends them but
// does not describe them in the OpenAPI document, so they are written here by hand from
// internal/api/janitor_stream.go. The job's state is the job hub's and not the run's: a run whose
// record says completed can have a job that ended failed or partial, because an item the engine kept
// in use ends failed in the hub. Word an outcome from the run record, never from this.
export type JanitorJobItemState =
  | "waiting"
  | "running"
  | "done"
  | "failed"
  | "skipped"
  | "cancelled";

export interface JanitorJobItemFrame {
  // The type of the object, its node and its engine ID, joined by slashes.
  id: string;
  // The object's name.
  label: string;
  // container, image or network, or step for the one item of a run that could not plan.
  kind: string;
  state: JanitorJobItemState;
  code: string;
  message: string;
  // The node and the engine ID of the object, once the removal ended.
  result: Record<string, string>;
}

export interface JanitorJobFrame {
  id: string;
  runId: string;
  state: "running" | "done" | "partial" | "failed" | "cancelled";
  trigger: "schedule" | "manual";
  by: string;
  createdAt: string;
  // Empty while the job runs.
  finishedAt: string;
  // How many removals have ended, and how many the run planned.
  done: number;
  total: number;
  items: JanitorJobItemFrame[];
}

// How often a run that is waiting or working is read again while its stream is not live. A run that
// has ended is not read again.
export const RUN_REFETCH_MS = 2_000;

// How often it is read again while its stream is live: the stream tells the page when the plan is
// known and when the run ends, so this only keeps the counts and the items table from going stale.
export const RUN_LIVE_REFETCH_MS = 5_000;

// The key of one run, so the run page and the list of runs can be made stale together.
export function janitorRunKey(envId: string, runId: string) {
  return ["env", envId, "janitor", "runs", runId] as const;
}

// JanitorRequestError is a janitor call the server refused or never answered. status is 0 when the
// server could not be reached; type is the problem type it named (operation_locked,
// janitor_run_not_found, too_many_jobs ...); fields hold what the problem names (kind, runId, job,
// started and stack of the operation that holds the environment).
export class JanitorRequestError extends Error {
  readonly status: number;
  readonly type?: string;
  readonly fields: ProblemField[];
  constructor(method: string, status: number, type?: string, fields: ProblemField[] = []) {
    super(`${method} janitor failed (${status})`);
    this.name = "JanitorRequestError";
    this.status = status;
    this.type = type;
    this.fields = fields;
  }

  // fieldOf is what the problem named at a location, empty when it named none.
  fieldOf(location: string): string {
    return this.fields.find((f) => f.location === location)?.message ?? "";
  }

  get operationLocked(): boolean {
    return this.status === 409 && this.type === "operation_locked";
  }

  get notFound(): boolean {
    return this.status === 404;
  }
}

function failureOf(method: string, res: { error?: unknown; response: Response }) {
  const problem = readProblem(res.error, res.response);
  return new JanitorRequestError(method, problem.status, problem.type, problem.fields);
}

// useJanitorPreview asks for a dry run. It is a mutation and not a query on purpose: the preview is
// computed from Docker at the moment it is asked for, and nothing may ask for it again in the
// background, because that would reset the ticks under a person's hands. The sheet calls it on open
// and on Refresh preview, and it is not retried.
export function useJanitorPreview(envId: string) {
  return useMutation<JanitorPreview, JanitorRequestError, void>({
    retry: false,
    gcTime: 0,
    mutationFn: async () => {
      try {
        const res = await api.POST("/api/env/{envId}/janitor/preview", {
          params: { path: { envId } },
        });
        if (!res.data) throw failureOf("POST", res);
        return res.data;
      } catch (e) {
        // A network failure has no status.
        throw e instanceof JanitorRequestError ? e : new JanitorRequestError("POST", 0);
      }
    },
  });
}

// useStartJanitorRun removes the ticked items. It is never retried (P2 D-42): a lost answer may mean
// the run started, and the run's page says. The server re-checks every item before it removes it.
export function useStartJanitorRun(envId: string) {
  const queryClient = useQueryClient();
  return useMutation<JanitorRunStarted, JanitorRequestError, JanitorItemRef[]>({
    retry: false,
    mutationFn: async (items) => {
      try {
        const res = await api.POST("/api/env/{envId}/janitor/runs", {
          params: { path: { envId } },
          body: { items },
        });
        if (!res.data) throw failureOf("POST", res);
        return res.data;
      } catch (e) {
        throw e instanceof JanitorRequestError ? e : new JanitorRequestError("POST", 0);
      }
    },
    onSettled: () => queryClient.invalidateQueries({ queryKey: ["env", envId, "janitor"] }),
  });
}

// One run: what started it, how it ended and every item it attempted. While it is waiting for the
// environment's lock or working it is read again every 2 s; once it has ended it is not. A run that
// is gone (404) is not asked for again.
export function janitorRunQuery(envId: string, runId: string) {
  return queryOptions({
    queryKey: janitorRunKey(envId, runId),
    queryFn: async (): Promise<JanitorRun> => {
      try {
        const res = await api.GET("/api/env/{envId}/janitor/runs/{runId}", {
          params: { path: { envId, runId } },
        });
        if (!res.data) throw failureOf("GET", res);
        return res.data;
      } catch (e) {
        throw e instanceof JanitorRequestError ? e : new JanitorRequestError("GET", 0);
      }
    },
    retry: (count, error) => !(error instanceof JanitorRequestError && error.notFound) && count < 2,
    refetchInterval: (query) => runPollMs(query.state.data?.state, false),
  });
}

// runPollMs is how long to wait before reading a run again: never for a run that has ended (or is not
// known yet), and for one that waits or works, 2 s, or 5 s while its stream is live.
export function runPollMs(state: JanitorRun["state"] | undefined, streamLive: boolean) {
  if (state !== "waiting" && state !== "running") return false;
  return streamLive ? RUN_LIVE_REFETCH_MS : RUN_REFETCH_MS;
}

// useJanitorRun reads one run and follows it while it is waiting or working.
export function useJanitorRun(envId: string, runId: string) {
  return useQuery(janitorRunQuery(envId, runId));
}

// lockOfError is the operation that holds the environment, as the 409 operation_locked names it: a
// stack operation names its stack, job and start; the janitor's hold names its kind and run. The
// result is what OperationLockAlert draws.
export function lockOfError(err: JanitorRequestError): StackLock {
  const janitor = err.fieldOf("kind") === "janitor";
  return {
    kind: janitor ? "janitor" : "deploy",
    ...(janitor ? { runId: err.fieldOf("runId") } : {}),
    stackId: err.fieldOf("stackId"),
    stackName: err.fieldOf("stack"),
    jobId: err.fieldOf("job"),
    startedAt: err.fieldOf("started"),
    startedBy: "",
  };
}

// The key of one item's crash evidence.
export function janitorEvidenceKey(envId: string, runId: string, seq: number) {
  return [...janitorRunKey(envId, runId), "evidence", seq] as const;
}

// The crash evidence of one container a run removed. A 404 is the answer "none was saved" (a
// container that did not crash, or evidence removed with its run) and is not asked for again.
export function janitorEvidenceQuery(envId: string, runId: string, seq: number) {
  return queryOptions({
    queryKey: janitorEvidenceKey(envId, runId, seq),
    queryFn: async (): Promise<JanitorEvidence> => {
      try {
        const res = await api.GET("/api/env/{envId}/janitor/runs/{runId}/items/{seq}/evidence", {
          params: { path: { envId, runId, seq } },
        });
        if (!res.data) throw failureOf("GET", res);
        return res.data;
      } catch (e) {
        throw e instanceof JanitorRequestError ? e : new JanitorRequestError("GET", 0);
      }
    },
    retry: (count, error) => !(error instanceof JanitorRequestError && error.notFound) && count < 2,
    // Evidence is saved once and never changes.
    staleTime: Number.POSITIVE_INFINITY,
  });
}

// useJanitorEvidence reads the evidence of an item while the sheet that shows it is open (seq is
// undefined when it is not).
export function useJanitorEvidence(envId: string, runId: string, seq: number | undefined) {
  return useQuery({
    ...janitorEvidenceQuery(envId, runId, seq ?? 0),
    enabled: seq !== undefined,
  });
}

// The janitor's overview of an environment: its status and the list of its runs. Both sit under one
// key prefix so the events feed can make them stale together on a container, image, network or
// service event (a removal or a new stale object changes what the status and the history say).
export function janitorOverviewKey(envId: string) {
  return ["env", envId, "janitor", "overview"] as const;
}

export function janitorStatusKey(envId: string) {
  return [...janitorOverviewKey(envId), "status"] as const;
}

export function janitorHistoryKey(envId: string, limit: number) {
  return [...janitorOverviewKey(envId), "history", limit] as const;
}

// The runs the history shows to begin with, how many each "Show 25 more" adds, and the most the
// server answers.
export const HISTORY_STEP = 25;
export const HISTORY_MAX = 100;

// janitorStatusQuery is whether the schedule runs the janitor on an environment, when it runs next,
// the newest run that has ended and the run that is going. Docker is the source of truth, so it is
// read again on the events described above and every 30 s while the tab is visible (TanStack does
// not poll a hidden tab).
export function janitorStatusQuery(envId: string) {
  return queryOptions({
    queryKey: janitorStatusKey(envId),
    queryFn: async (): Promise<JanitorStatus> => {
      try {
        const res = await api.GET("/api/env/{envId}/janitor", { params: { path: { envId } } });
        if (!res.data) throw failureOf("GET", res);
        return res.data;
      } catch (e) {
        throw e instanceof JanitorRequestError ? e : new JanitorRequestError("GET", 0);
      }
    },
    refetchInterval: LIST_REFETCH_MS,
  });
}

// useJanitorStatus reads the status of an environment's janitor; enabled is false for a person who
// may not read the environment.
export function useJanitorStatus(envId: string, enabled = true) {
  return useQuery({ ...janitorStatusQuery(envId), enabled });
}

// janitorRunsQuery is the newest `limit` runs of an environment, without their items. The rows
// already on screen stay while a longer list is on its way, so "Show 25 more" does not blank the
// table.
export function janitorRunsQuery(envId: string, limit: number) {
  return queryOptions({
    queryKey: janitorHistoryKey(envId, limit),
    queryFn: async (): Promise<JanitorRunList> => {
      try {
        const res = await api.GET("/api/env/{envId}/janitor/runs", {
          params: { path: { envId }, query: { limit } },
        });
        if (!res.data) throw failureOf("GET", res);
        return res.data;
      } catch (e) {
        throw e instanceof JanitorRequestError ? e : new JanitorRequestError("GET", 0);
      }
    },
    placeholderData: keepPreviousData,
    refetchInterval: LIST_REFETCH_MS,
  });
}

// useJanitorRuns reads the run history; enabled is false for a person who may not read the
// environment.
export function useJanitorRuns(envId: string, limit: number, enabled = true) {
  return useQuery({ ...janitorRunsQuery(envId, limit), enabled });
}

// The key of the clean-ups the Jobs indicator lists.
export const janitorJobsKey = ["janitor-jobs"] as const;

// Polled every 3 seconds while a clean-up works and every 30 otherwise, as the stack jobs list is.
export const JANITOR_JOBS_POLL_RUNNING_MS = 3_000;
export const JANITOR_JOBS_POLL_IDLE_MS = 30_000;

// janitorJobsQuery lists the clean-ups the caller may follow: those working or ended in the last ten
// minutes, on the environments they may read, a scheduled one included. A poll that fails keeps the
// last list, so the indicator shows no error chrome for it. A job's own state is the job hub's and
// not the run's: the indicator words a finished clean-up from the run record (see the frames above).
export const janitorJobsQuery = queryOptions({
  queryKey: janitorJobsKey,
  queryFn: async (): Promise<JanitorJobSummary[]> => {
    const res = await api.GET("/api/janitor-jobs");
    if (!res.data) throw failureOf("GET", res);
    return res.data.jobs ?? [];
  },
  refetchInterval: (query) =>
    query.state.data?.some((j) => j.state === "running")
      ? JANITOR_JOBS_POLL_RUNNING_MS
      : JANITOR_JOBS_POLL_IDLE_MS,
  retry: false,
});

// useJanitorJobs reads the clean-ups for the Jobs indicator.
export function useJanitorJobs() {
  return useQuery(janitorJobsQuery);
}
