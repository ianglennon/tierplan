// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import {
  keepPreviousData,
  queryOptions,
  useMutation,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import { useEffect, useState } from "react";

import { addressSpaceKey } from "@/api/address-space";
import { api } from "@/api/client";
import { type ProblemField, readProblem } from "@/api/problem";
import { LIST_REFETCH_MS } from "@/api/resources";
import type { components } from "@/api/schema";
import { type StackDetail, type StackLock, stackKey } from "@/api/stacks";

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
export type JanitorPolicy = components["schemas"]["JanitorPolicyBody"];
export type JanitorPolicyFields = components["schemas"]["JanitorPolicyFields"];
export type JanitorPolicyRequest = components["schemas"]["JanitorPolicyRequest"];
export type SchedulePreview = components["schemas"]["SchedulePreviewBody"];
export type JanitorDefaults = components["schemas"]["JanitorDefaultsBody"];
export type JanitorDefaultsRequest = components["schemas"]["JanitorDefaultsRequest"];
export type JanitorRetentionPreview = components["schemas"]["JanitorRetentionPreviewBody"];
export type SchedulePreset = NonNullable<JanitorPolicyFields["schedulePreset"]>;
export type JanitorNode = components["schemas"]["JanitorNodeBody"];
export type JanitorNodeRequest = components["schemas"]["JanitorNodeRequest"];
export type TaskHistory = components["schemas"]["TaskHistoryBody"];
export type TaskHistoryChange = components["schemas"]["TaskHistoryChangeBody"];
export type JanitorKeepTarget = components["schemas"]["JanitorKeepTarget"];
export type JanitorKept = components["schemas"]["JanitorKeptItem"];
export type StackJanitor = components["schemas"]["StackJanitorBody"];

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

// What a preview is asked about when it is not the saved policy: a candidate policy (the fields a
// save of the policy card would store) and/or candidate settings of swarm nodes, by swarm node ID,
// each exactly as saving that node would store it. Nothing is saved either way.
export interface JanitorPreviewCandidate {
  policy?: JanitorPolicyFields;
  nodes?: Record<string, JanitorNodeRequest>;
}

// useJanitorPreview asks for a dry run. It is a mutation and not a query on purpose: the preview is
// computed from Docker at the moment it is asked for, and nothing may ask for it again in the
// background, because that would reset the ticks under a person's hands. The sheet calls it on open
// and on Refresh preview, and it is not retried. The variable is a candidate: with one the preview
// shows what that policy and those node settings would remove, without it the saved policy.
export function useJanitorPreview(envId: string) {
  return useMutation<JanitorPreview, JanitorRequestError, JanitorPreviewCandidate | undefined>({
    retry: false,
    gcTime: 0,
    mutationFn: async (candidate) => {
      try {
        const body =
          candidate && (candidate.policy || candidate.nodes)
            ? {
                ...(candidate.policy ? { policy: candidate.policy } : {}),
                ...(candidate.nodes ? { nodes: candidate.nodes } : {}),
              }
            : undefined;
        const res = await api.POST("/api/env/{envId}/janitor/preview", {
          params: { path: { envId } },
          ...(body ? { body } : {}),
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

// The key of one environment's janitor policy. The schedule previews sit under it, so one
// invalidation of the policy makes them stale together.
export function janitorPolicyKey(envId: string) {
  return ["env", envId, "janitor", "policy"] as const;
}

// janitorPolicyQuery is an environment's janitor policy: for each field the value that applies and
// whether the environment set it itself, the instance defaults, the version to send back with a save
// and the server's time zone. The defaults come with it because the instance defaults route is for
// instance admins only. It is read when the page opens and when the window regains focus, not on a
// timer: a person's unsaved edits are kept against the snapshot they began from.
export function janitorPolicyQuery(envId: string) {
  return queryOptions({
    queryKey: janitorPolicyKey(envId),
    queryFn: async (): Promise<JanitorPolicy> => {
      try {
        const res = await api.GET("/api/env/{envId}/janitor/policy", {
          params: { path: { envId } },
        });
        if (!res.data) throw failureOf("GET", res);
        return res.data;
      } catch (e) {
        throw e instanceof JanitorRequestError ? e : new JanitorRequestError("GET", 0);
      }
    },
  });
}

// useJanitorPolicy reads the policy; enabled is false for a person who may not read the environment.
export function useJanitorPolicy(envId: string, enabled = true) {
  return useQuery({ ...janitorPolicyQuery(envId), enabled });
}

// useSaveJanitorPolicy saves the whole policy (a PUT is a full replace: a field that is null follows
// the default). It is never retried (P2 D-42). The answer is the policy as saved and replaces what the
// page holds; the status is made stale because the schedule or the switch may have moved the next run.
// A save made against another version is 409 settings_changed, and an unusable schedule 422
// invalid_schedule or schedule_too_frequent.
export function useSaveJanitorPolicy(envId: string) {
  const queryClient = useQueryClient();
  return useMutation<JanitorPolicy, JanitorRequestError, JanitorPolicyRequest>({
    retry: false,
    mutationFn: async (body) => {
      try {
        const res = await api.PUT("/api/env/{envId}/janitor/policy", {
          params: { path: { envId } },
          body,
        });
        if (!res.data) throw failureOf("PUT", res);
        return res.data;
      } catch (e) {
        throw e instanceof JanitorRequestError ? e : new JanitorRequestError("PUT", 0);
      }
    },
    onSuccess: (saved) => queryClient.setQueryData(janitorPolicyKey(envId), saved),
    onSettled: () => queryClient.invalidateQueries({ queryKey: janitorOverviewKey(envId) }),
  });
}

// conflictOf is who saved the policy last, as a 409 settings_changed names them. Empty when the
// server did not know the person.
export function conflictOf(err: JanitorRequestError): string {
  return err.fieldOf("changedBy");
}

// patchNode puts a node as the server answered it into the policy the page holds, in place, so the
// table follows a change without a read and the policy's own version is left alone (a node's settings
// are stored apart from the environment's).
function patchNode(
  queryClient: ReturnType<typeof useQueryClient>,
  envId: string,
  node: JanitorNode,
) {
  queryClient.setQueryData<JanitorPolicy>(janitorPolicyKey(envId), (old) =>
    old
      ? { ...old, nodes: (old.nodes ?? []).map((n) => (n.nodeId === node.nodeId ? node : n)) }
      : old,
  );
}

// useSaveJanitorNode sets one swarm node's two settings: whether the janitor cleans it and how many
// exited task containers of a slot it keeps. A PUT replaces the node's settings, so a field that is
// null follows the environment and the caller sends the one it is not changing as it stands. It is
// never retried (P2 D-42). The answer is the node as stored. A node the swarm no longer has is 404
// node_not_found, and a value out of range is huma's own 422.
export function useSaveJanitorNode(envId: string) {
  const queryClient = useQueryClient();
  return useMutation<
    JanitorNode,
    JanitorRequestError,
    { nodeId: string; body: JanitorNodeRequest }
  >({
    retry: false,
    mutationFn: async ({ nodeId, body }) => {
      try {
        const res = await api.PUT("/api/env/{envId}/janitor/nodes/{nodeId}", {
          params: { path: { envId, nodeId } },
          body,
        });
        if (!res.data) throw failureOf("PUT", res);
        return res.data;
      } catch (e) {
        throw e instanceof JanitorRequestError ? e : new JanitorRequestError("PUT", 0);
      }
    },
    onSuccess: (node) => patchNode(queryClient, envId, node),
  });
}

// useResetJanitorNode returns one swarm node to following the environment (it removes the node's
// stored settings). It is never retried (P2 D-42).
export function useResetJanitorNode(envId: string) {
  const queryClient = useQueryClient();
  return useMutation<JanitorNode, JanitorRequestError, string>({
    retry: false,
    mutationFn: async (nodeId) => {
      try {
        const res = await api.DELETE("/api/env/{envId}/janitor/nodes/{nodeId}", {
          params: { path: { envId, nodeId } },
        });
        if (!res.data) throw failureOf("DELETE", res);
        return res.data;
      } catch (e) {
        throw e instanceof JanitorRequestError ? e : new JanitorRequestError("DELETE", 0);
      }
    },
    onSuccess: (node) => patchNode(queryClient, envId, node),
  });
}

// How long after the last keystroke the server is asked what a cron expression would do. The server
// holds a person to 120 calls a minute, so typing never gets near it.
export const SCHEDULE_DEBOUNCE_MS = 400;

export interface SchedulePreviewState {
  // The server's answer for the expression as it stands now. Undefined while the person is still
  // typing, while the answer is on its way, for a blank expression and when the question failed.
  data: SchedulePreview | undefined;
  // True from a keystroke until the answer to it has come.
  waiting: boolean;
}

// useSchedulePreview asks the server which times an expression would run at, 400 ms after the last
// change. The times are the scheduler's own (it does the cron arithmetic, with the environment's
// start offset); the browser does none. It reads only, so asking again is harmless. enabled is false
// for a preset, whose times need no question.
export function useSchedulePreview(
  envId: string,
  schedule: string,
  enabled = true,
): SchedulePreviewState {
  const typed = schedule.trim();
  const [settled, setSettled] = useState(typed);
  useEffect(() => {
    if (typed === settled) return;
    const id = setTimeout(() => setSettled(typed), SCHEDULE_DEBOUNCE_MS);
    return () => clearTimeout(id);
  }, [typed, settled]);
  const asking = enabled && settled !== "";
  const query = useQuery({
    queryKey: [...janitorPolicyKey(envId), "schedule-preview", settled] as const,
    queryFn: async (): Promise<SchedulePreview> => {
      const res = await api.POST("/api/env/{envId}/janitor/schedule-preview", {
        params: { path: { envId } },
        body: { schedule: settled },
      });
      if (!res.data) throw failureOf("POST", res);
      return res.data;
    },
    enabled: asking,
    retry: false,
    staleTime: 30_000,
  });
  const current = typed === settled;
  return {
    data: asking && current ? query.data : undefined,
    waiting: enabled && typed !== "" && (!current || query.isFetching),
  };
}

// The key of the instance-wide janitor defaults (Settings, General). It sits outside the "env" keys
// because it belongs to no environment.
export const janitorDefaultsKey = ["settings", "janitor"] as const;

// janitorDefaultsQuery is what every environment's janitor follows until the environment sets the
// field itself, the run-history retention, the version a save must send back and how many
// environments follow at least one default. Reading needs settings.read. It is read when the page
// opens and when the window regains focus, not on a timer: a person's unsaved edits are kept against
// the snapshot they began from.
export function janitorDefaultsQuery() {
  return queryOptions({
    queryKey: janitorDefaultsKey,
    queryFn: async (): Promise<JanitorDefaults> => {
      try {
        const res = await api.GET("/api/settings/janitor");
        if (!res.data) throw failureOf("GET", res);
        return res.data;
      } catch (e) {
        throw e instanceof JanitorRequestError ? e : new JanitorRequestError("GET", 0);
      }
    },
  });
}

// useJanitorDefaults reads the instance-wide janitor defaults.
export function useJanitorDefaults() {
  return useQuery(janitorDefaultsQuery());
}

// useSaveJanitorDefaults saves every default and the retention (a PUT is a full replace). It is never
// retried (P2 D-42): a lowered retention deletes run records. The answer replaces what the page
// holds. A save changes what every environment that follows a field does, and what a lowered
// retention keeps, so each environment's janitor reads (policy, status, run history) are made
// stale. A save made against another version is 409 settings_changed, an unusable schedule 422
// invalid_schedule or schedule_too_frequent, and a retention out of range 422 retention_out_of_range.
export function useSaveJanitorDefaults() {
  const queryClient = useQueryClient();
  return useMutation<JanitorDefaults, JanitorRequestError, JanitorDefaultsRequest>({
    retry: false,
    mutationFn: async (body) => {
      try {
        const res = await api.PUT("/api/settings/janitor", { body });
        if (!res.data) throw failureOf("PUT", res);
        return res.data;
      } catch (e) {
        throw e instanceof JanitorRequestError ? e : new JanitorRequestError("PUT", 0);
      }
    },
    onSuccess: (saved) => queryClient.setQueryData(janitorDefaultsKey, saved),
    onSettled: () =>
      queryClient.invalidateQueries({
        predicate: (q) => q.queryKey[0] === "env" && q.queryKey[2] === "janitor",
      }),
  });
}

// janitorRetentionPreviewQuery is how many run records, in how many environments, saving a retention
// of days and runs would delete now. It is a count taken at the moment of asking, so it is never
// cached: the confirm that shows it must not show an older answer. It needs settings.write.
export function janitorRetentionPreviewQuery(days: number, runs: number) {
  return queryOptions({
    queryKey: [...janitorDefaultsKey, "retention-preview", days, runs] as const,
    queryFn: async (): Promise<JanitorRetentionPreview> => {
      try {
        const res = await api.GET("/api/settings/janitor/retention-preview", {
          params: { query: { days, runs } },
        });
        if (!res.data) throw failureOf("GET", res);
        return res.data;
      } catch (e) {
        throw e instanceof JanitorRequestError ? e : new JanitorRequestError("GET", 0);
      }
    },
    staleTime: 0,
    gcTime: 0,
    retry: false,
  });
}

// useRetentionPreview gives the function the defaults card asks the count with when a lowered
// retention is about to be saved: (days, runs) -> what that retention would delete now. The server
// answers 0 for a retention that does not lower the stored one.
export function useRetentionPreview() {
  const queryClient = useQueryClient();
  return (days: number, runs: number) =>
    queryClient.fetchQuery(janitorRetentionPreviewQuery(days, runs));
}

// The key of a swarm's task history limit. It sits under the environment's janitor keys but outside
// the overview, which the events feed makes stale: the limit changes only when someone changes it.
export function taskHistoryKey(envId: string) {
  return ["env", envId, "janitor", "task-history"] as const;
}

// taskHistoryQuery is how many finished tasks per slot the swarm keeps, read live from a manager. A
// swarm with no manager connected answers 503 no_manager, an older agent 502 agent_policy_denied, and
// a standalone host 409 not_swarm; none of them is asked for again. It is read when the page opens and
// when the window regains focus, not on a timer: a person's unsaved number is kept against it.
export function taskHistoryQuery(envId: string) {
  return queryOptions({
    queryKey: taskHistoryKey(envId),
    queryFn: async (): Promise<TaskHistory> => {
      try {
        const res = await api.GET("/api/env/{envId}/janitor/task-history", {
          params: { path: { envId } },
        });
        if (!res.data) throw failureOf("GET", res);
        return res.data;
      } catch (e) {
        throw e instanceof JanitorRequestError ? e : new JanitorRequestError("GET", 0);
      }
    },
    retry: false,
  });
}

// useTaskHistory reads the swarm's task history limit; enabled is false for a standalone host and for
// a person who may not read the environment.
export function useTaskHistory(envId: string, enabled = true) {
  return useQuery({ ...taskHistoryQuery(envId), enabled });
}

// useSaveTaskHistory sets the swarm's task history limit (0 to 1000). It is never retried (P2 D-42):
// it is a swarm update. The answer says whether anything was changed and what the swarm now keeps, and
// replaces what the page holds. A swarm that changed under the save is 409 swarm_changed, no manager
// 503 no_manager, an older agent 502 agent_policy_denied and a read-only host 409 host_read_only.
export function useSaveTaskHistory(envId: string) {
  const queryClient = useQueryClient();
  return useMutation<TaskHistoryChange, JanitorRequestError, number>({
    retry: false,
    mutationFn: async (limit) => {
      try {
        const res = await api.PUT("/api/env/{envId}/janitor/task-history", {
          params: { path: { envId } },
          body: { limit },
        });
        if (!res.data) throw failureOf("PUT", res);
        return res.data;
      } catch (e) {
        throw e instanceof JanitorRequestError ? e : new JanitorRequestError("PUT", 0);
      }
    },
    onSuccess: ({ limit, dockerDefault, keepsAll }) =>
      queryClient.setQueryData<TaskHistory>(taskHistoryKey(envId), {
        limit,
        dockerDefault,
        keepsAll,
      }),
  });
}

// The key of an environment's kept items. It sits under the overview, which the events feed makes
// stale on a container, image or network event: a kept object that is removed by hand, or made again,
// changes its `present` flag.
export function janitorKeepsKey(envId: string) {
  return [...janitorOverviewKey(envId), "keeps"] as const;
}

// janitorKeepsQuery is everything the janitor never removes in an environment because a person chose
// Keep on it or it carries the tierplan.keep label (one item per object; the label wins). Reading
// needs environments.read.
export function janitorKeepsQuery(envId: string) {
  return queryOptions({
    queryKey: janitorKeepsKey(envId),
    queryFn: async (): Promise<JanitorKept[]> => {
      try {
        const res = await api.GET("/api/env/{envId}/janitor/keeps", {
          params: { path: { envId } },
        });
        if (!res.data) throw failureOf("GET", res);
        return res.data.items ?? [];
      } catch (e) {
        throw e instanceof JanitorRequestError ? e : new JanitorRequestError("GET", 0);
      }
    },
  });
}

// useJanitorKeeps reads the kept items; enabled is false for a person who may not read the environment.
export function useJanitorKeeps(envId: string, enabled = true) {
  return useQuery({ ...janitorKeepsQuery(envId), enabled });
}

// A keep or an unkeep changes three things that show the same fact: the kept list, the next preview
// and, for a network, the Address space page (one table, D-12).
function afterKeepChange(queryClient: ReturnType<typeof useQueryClient>, envId: string) {
  void queryClient.invalidateQueries({ queryKey: janitorKeepsKey(envId) });
  void queryClient.invalidateQueries({ queryKey: addressSpaceKey(envId) });
}

// useKeepJanitorItem keeps containers, images and networks out of every cleanup, by a schedule and
// by Clean now. It is never retried (P2 D-42). A target the engine no longer has is 404 not_found,
// one on several nodes without a node 422 node_required. It answers how many keeps are new.
export function useKeepJanitorItem(envId: string) {
  const queryClient = useQueryClient();
  return useMutation<number, JanitorRequestError, JanitorKeepTarget[]>({
    retry: false,
    mutationFn: async (targets) => {
      try {
        const res = await api.PUT("/api/env/{envId}/janitor/keeps", {
          params: { path: { envId } },
          body: { targets },
        });
        if (!res.data) throw failureOf("PUT", res);
        return res.data.kept;
      } catch (e) {
        throw e instanceof JanitorRequestError ? e : new JanitorRequestError("PUT", 0);
      }
    },
    onSettled: () => afterKeepChange(queryClient, envId),
  });
}

// useUnkeepJanitorItem takes a person's keep back. It is never retried (P2 D-42). 409 kept_by_label
// means the object carries the tierplan.keep label, which only its owner can remove; 404
// keep_not_found means nobody is keeping it (it was already taken back).
export function useUnkeepJanitorItem(envId: string) {
  const queryClient = useQueryClient();
  return useMutation<void, JanitorRequestError, Pick<JanitorKept, "kind" | "nodeId" | "ref">>({
    retry: false,
    mutationFn: async ({ kind, nodeId, ref }) => {
      try {
        const res = await api.DELETE("/api/env/{envId}/janitor/keeps", {
          params: { path: { envId }, query: { kind, ref, ...(nodeId ? { nodeId } : {}) } },
        });
        if (!res.response.ok) throw failureOf("DELETE", res);
      } catch (e) {
        throw e instanceof JanitorRequestError ? e : new JanitorRequestError("DELETE", 0);
      }
    },
    onSettled: () => afterKeepChange(queryClient, envId),
  });
}

// useSetStackJanitor switches a stack's exclusion from the janitor on or off. It is never retried
// (P2 D-42) and is a plain PUT that applies at once. The answer (excluded and declared) is put into the
// stack the page holds, so the switch follows the server's word rather than the click; a refusal
// leaves the stack as it was. 404 stack_not_found when the stack is gone.
export function useSetStackJanitor(envId: string, stackId: string) {
  const queryClient = useQueryClient();
  return useMutation<StackJanitor, JanitorRequestError, boolean>({
    retry: false,
    mutationFn: async (excluded) => {
      try {
        const res = await api.PUT("/api/env/{envId}/stacks/{stackId}/janitor", {
          params: { path: { envId, stackId } },
          body: { excluded },
        });
        if (!res.data) throw failureOf("PUT", res);
        return res.data;
      } catch (e) {
        throw e instanceof JanitorRequestError ? e : new JanitorRequestError("PUT", 0);
      }
    },
    onSuccess: (janitor) =>
      queryClient.setQueryData<StackDetail>(stackKey(envId, stackId), (old) =>
        old ? { ...old, janitor } : old,
      ),
    onSettled: () => queryClient.invalidateQueries({ queryKey: janitorOverviewKey(envId) }),
  });
}
