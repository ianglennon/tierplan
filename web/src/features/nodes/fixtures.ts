// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { components } from "@/api/schema";

// What the node page's tests share: a node and its tasks as the server answers them.

export type NodeDetailBody = components["schemas"]["NodeDetailBody"];
export type NodeTaskBody = components["schemas"]["NodeTaskItem"];
export type NodeTasksAnswer = components["schemas"]["NodeTasksBody"];

export const NODE_ENV = "e1";
export const NODE_ID = "n1";

export function nodeDetail(extra: Partial<NodeDetailBody> = {}): NodeDetailBody {
  return {
    id: NODE_ID,
    version: 12,
    hostname: "mgr-1",
    role: "manager",
    leader: true,
    runsBayplan: true,
    state: "ready",
    availability: "active",
    reachability: "reachable",
    address: "10.0.0.1",
    managerAddr: "10.0.0.1:2377",
    engineVersion: "29.1.0",
    os: "linux",
    arch: "aarch64",
    cpus: 8,
    memoryBytes: 31 * 1024 ** 3,
    labels: { disk: "ssd", zone: "a" },
    engineLabels: { provider: "homelab" },
    createdAt: "2026-09-01T10:00:00Z",
    updatedAt: "2026-10-02T11:30:00Z",
    agent: { reporting: true, version: "0.1.0", lastSeen: "2026-10-14T09:00:00Z" },
    targetNodeId: "t1",
    ...extra,
  };
}

export function nodeTask(extra: Partial<NodeTaskBody> = {}): NodeTaskBody {
  return {
    serviceId: "svc-web",
    service: "web",
    id: "task1abcdef12345678",
    slot: 2,
    nodeId: NODE_ID,
    node: "mgr-1",
    nodeKnown: true,
    targetNodeId: "t1",
    state: "running",
    dockerState: "running",
    desiredState: "running",
    error: "",
    message: "started",
    exitCode: null,
    containerId: "c1",
    createdAt: "2026-10-14T08:00:00Z",
    updatedAt: "2026-10-14T08:00:05Z",
    stateSince: "2026-10-14T08:00:05Z",
    ...extra,
  };
}
