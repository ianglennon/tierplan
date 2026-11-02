// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import type { JanitorItem, JanitorPreview, JanitorRun, JanitorRunItem } from "@/api/janitor";

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
