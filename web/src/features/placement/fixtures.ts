// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import type {
  PlacementData,
  PlacementNode,
  PlacementService,
  PlacementTask,
} from "@/features/placement/project";

// Test data for the placement tests: a node and a task with every field the API sends.

export const node = (extra: Partial<PlacementNode> = {}): PlacementNode => ({
  id: "n1",
  hostname: "mgr-1",
  role: "manager",
  leader: false,
  availability: "active",
  state: "ready",
  cpus: 4,
  memoryBytes: 8 * 1024 ** 3,
  address: "10.0.0.1",
  os: "linux",
  arch: "x86_64",
  labels: {},
  engineLabels: {},
  targetNodeId: "",
  ...extra,
});

export const task = (extra: Partial<PlacementTask> = {}): PlacementTask => ({
  id: "task-1",
  serviceId: "svc-web",
  service: "web",
  stack: "shop",
  slot: 1,
  nodeId: "n1",
  nodeKnown: true,
  state: "running",
  dockerState: "running",
  desiredState: "running",
  error: "",
  ...extra,
});

export const service = (extra: Partial<PlacementService> = {}): PlacementService => ({
  id: "svc-web",
  name: "web",
  stack: "shop",
  mode: "replicated",
  constraints: [],
  preferences: [],
  maxReplicas: 0,
  ...extra,
});

export const placement = (
  nodes: PlacementNode[],
  tasks: PlacementTask[],
  extra: Partial<PlacementData> = {},
): PlacementData => ({ nodes, tasks, services: [], ...extra });
