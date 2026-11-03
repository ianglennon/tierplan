// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import type {
  JanitorDefaults,
  JanitorEvidence,
  JanitorItem,
  JanitorJobSummary,
  JanitorPolicy,
  JanitorPreview,
  JanitorRun,
  JanitorRunItem,
  JanitorRunSummary,
  JanitorStatus,
} from "@/api/janitor";

// Builders for the janitor's tests: a preview item, a preview, a run item and a run, with the fields
// a test does not care about filled in.

export function previewItem(over: Partial<JanitorItem> = {}): JanitorItem {
  return {
    ageSeconds: 3 * 86_400,
    class: "removable",
    name: "web.1.abc",
    node: "node-1",
    nodeId: "n1",
    reason: { code: "task_beyond_keep", service: "web", slot: "1", rank: 3 },
    ref: "c1",
    sizeBytes: 1_200_000,
    type: "container",
    ...over,
  };
}

export function preview(over: Partial<JanitorPreview> = {}): JanitorPreview {
  const items = over.items ?? [previewItem()];
  return {
    items,
    networksWithheld: false,
    nodesChecked: 2,
    notRemoved: [],
    previewedAt: new Date().toISOString(),
    summary: {
      containers: items.filter((i) => i.type === "container").length,
      networks: items.filter((i) => i.type === "network").length,
      images: items.filter((i) => i.type === "image").length,
      estBytes: null,
    },
    withheldNodes: [],
    ...over,
  };
}

export function runItem(over: Partial<JanitorRunItem> = {}): JanitorRunItem {
  return {
    code: "",
    hasEvidence: false,
    name: "web.1.abc",
    node: "node-1",
    nodeId: "n1",
    outcome: "removed",
    reason: { code: "task_beyond_keep", service: "web", slot: "1", rank: 3 },
    ref: "c1",
    seq: 1,
    sizeBytes: 1_200_000,
    type: "container",
    ...over,
  };
}

export function run(over: Partial<JanitorRun> = {}): JanitorRun {
  return {
    actor: "u1",
    actorName: "Ada",
    counts: {
      failed: 0,
      kept: 0,
      removedContainers: 0,
      removedImages: 0,
      removedNetworks: 0,
      skippedOffline: 0,
    },
    estBytes: null,
    finishedAt: new Date().toISOString(),
    id: "r1",
    items: [],
    jobId: "j1",
    reason: "",
    startedAt: new Date(Date.now() - 60_000).toISOString(),
    state: "completed",
    trigger: "manual",
    ...over,
  };
}

// The crash evidence of a container the janitor removed.
export function evidence(over: Partial<JanitorEvidence> = {}): JanitorEvidence {
  return {
    error: "",
    exitCode: 137,
    finishedAt: "2026-11-03T10:05:00Z",
    image: "registry.example/web:1.4",
    imageDigest: "registry.example/web@sha256:abc123",
    imageId: "sha256:def456",
    logLines: ["2026-11-03T10:04:58Z starting", "2026-11-03T10:04:59Z out of memory"],
    logState: "saved",
    node: "node-1",
    oomKilled: true,
    startedAt: "2026-11-03T10:00:00Z",
    ...over,
  };
}

// A run as the history lists it and the status names it: no items.
export function runSummary(over: Partial<JanitorRunSummary> = {}): JanitorRunSummary {
  return {
    actor: "u1",
    actorName: "Ada",
    counts: {
      failed: 0,
      kept: 0,
      removedContainers: 0,
      removedImages: 0,
      removedNetworks: 0,
      skippedOffline: 0,
    },
    durationSeconds: 12,
    estBytes: null,
    finishedAt: new Date(Date.now() - 3_588_000).toISOString(),
    id: "r1",
    jobId: "j1",
    reason: "",
    startedAt: new Date(Date.now() - 3_600_000).toISOString(),
    state: "completed",
    trigger: "manual",
    ...over,
  };
}

// The status of an environment's janitor: on, daily at 03:00 in Europe/London, next run in five hours.
export function janitorStatus(over: Partial<JanitorStatus> = {}): JanitorStatus {
  return {
    enabled: true,
    nextRunAt: new Date(Date.now() + 5 * 3_600_000).toISOString(),
    retention: { days: 90, runs: 100 },
    schedule: "0 3 * * *",
    schedulePreset: "daily",
    timezone: "Europe/London",
    ...over,
  };
}

// An environment's janitor policy as the server answers it: every field follows the instance default
// (on, daily at 03:00, keep 2, 24 hours, both opt-ins off, no swarm nodes) until a test overrides it. Pass `defaults`
// to change what the defaults are, and a field to give it an own value.
export function janitorPolicy(over: Partial<JanitorPolicy> = {}): JanitorPolicy {
  return {
    version: 3,
    timezone: "Europe/London",
    enabled: { value: true, overridden: false },
    schedule: { value: "0 3 * * *", preset: "daily", overridden: false },
    keepN: { value: 2, overridden: false },
    graceHours: { value: 24, overridden: false },
    removeUnusedNetworks: { value: false, overridden: false },
    removeStoppedContainers: { value: false, overridden: false },
    defaults: {
      enabled: true,
      schedule: "0 3 * * *",
      schedulePreset: "daily",
      keepN: 2,
      graceHours: 24,
      removeUnusedNetworks: false,
      removeStoppedContainers: false,
    },
    nodes: [],
    ...over,
  };
}

// A clean-up the Jobs indicator lists.
export function janitorJob(over: Partial<JanitorJobSummary> = {}): JanitorJobSummary {
  return {
    by: "Ada",
    done: 3,
    envId: "e1",
    jobId: "j1",
    runId: "r1",
    startedAt: new Date(Date.now() - 60_000).toISOString(),
    state: "running",
    total: 8,
    trigger: "manual",
    ...over,
  };
}

// The instance-wide janitor defaults as the server answers them: on, daily at 03:00 in Europe/London,
// keep 2, 24 hours, both opt-ins off, history kept 90 days or the last 100 runs, and three
// environments following them.
export function janitorDefaults(over: Partial<JanitorDefaults> = {}): JanitorDefaults {
  return {
    version: 4,
    timezone: "Europe/London",
    enabled: true,
    schedule: "0 3 * * *",
    schedulePreset: "daily",
    keepN: 2,
    graceHours: 24,
    removeUnusedNetworks: false,
    removeStoppedContainers: false,
    retentionDays: 90,
    retentionRuns: 100,
    followingEnvironments: 3,
    removedRuns: 0,
    ...over,
  };
}
