// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { describe, expect, it } from "vitest";

import type { EnvironmentItem, HealthState } from "@/api/environments";
import { SEVERITY, sortEnvironments } from "@/features/environments/sort";

function env(
  id: string,
  name: string,
  state: HealthState = "online",
  extra: Partial<EnvironmentItem> = {},
) {
  const item: EnvironmentItem = {
    id,
    name,
    kind: "agent",
    enabled: true,
    isSelfHost: false,
    isLocalSocket: false,
    createdAt: "2026-10-01T10:00:00Z",
    engine: {},
    status: { state },
    agentVersion: null,
    agents: null,
    engineVersion: null,
    failingServices: null,
    lastSeen: null,
    nodes: null,
    updateAvailable: false,
    ...extra,
  };
  return item;
}

const ids = (items: EnvironmentItem[]) => items.map((e) => e.id);

describe("SEVERITY", () => {
  it("is the UI-SPEC problems-first order, with unknown just after pending", () => {
    expect(SEVERITY).toEqual([
      "offline",
      "revoked",
      "cluster_changed",
      "degraded",
      "unsupported",
      "reconnecting",
      "pending",
      "unknown",
      "online",
    ]);
  });
});

describe("sortEnvironments", () => {
  it("puts offline before revoked before cluster_changed before degraded before unsupported before reconnecting before pending before online", () => {
    const shuffled = [
      env("1", "a", "online"),
      env("2", "a", "pending"),
      env("3", "a", "reconnecting"),
      env("4", "a", "unsupported"),
      env("5", "a", "degraded"),
      env("6", "a", "cluster_changed"),
      env("7", "a", "revoked"),
      env("8", "a", "offline"),
    ];
    expect(ids(sortEnvironments(shuffled))).toEqual(["8", "7", "6", "5", "4", "3", "2", "1"]);
  });

  it("sorts equal states by name without regard to case, then by ID", () => {
    const items = [env("3", "beta"), env("2", "Alpha"), env("1", "alpha"), env("4", "Beta")];
    expect(ids(sortEnvironments(items))).toEqual(["1", "2", "3", "4"]);
  });

  it("keeps both environments that share a name, in ID order, next to each other", () => {
    const items = [env("b", "prod"), env("a", "prod"), env("c", "dev"), env("d", "stage")];
    expect(ids(sortEnvironments(items))).toEqual(["c", "a", "b", "d"]);
  });

  it("does not change the list it was given", () => {
    const items = [env("2", "b"), env("1", "a")];
    sortEnvironments(items);
    expect(ids(items)).toEqual(["2", "1"]);
  });

  it("sorts by name ascending and descending, and breaks ties by ID", () => {
    const items = [env("2", "b", "offline"), env("1", "a"), env("4", "b"), env("3", "c")];
    expect(ids(sortEnvironments(items, "name", "asc"))).toEqual(["1", "2", "4", "3"]);
    expect(ids(sortEnvironments(items, "name", "desc"))).toEqual(["3", "2", "4", "1"]);
  });

  it("sorts by health with the worst first when ascending and the healthiest first when descending", () => {
    const items = [env("1", "a", "online"), env("2", "b", "offline"), env("3", "c", "degraded")];
    expect(ids(sortEnvironments(items, "health", "asc"))).toEqual(["2", "3", "1"]);
    expect(ids(sortEnvironments(items, "health", "desc"))).toEqual(["1", "3", "2"]);
  });

  it("sorts by nodes ready then total, and keeps environments with none last either way", () => {
    const items = [
      env("1", "a", "online", { nodes: { ready: 3, total: 4 } }),
      env("2", "b", "online", { nodes: null }),
      env("3", "c", "online", { nodes: { ready: 1, total: 1 } }),
      env("4", "d", "online", { nodes: { ready: 3, total: 3 } }),
    ];
    expect(ids(sortEnvironments(items, "nodes", "asc"))).toEqual(["3", "4", "1", "2"]);
    expect(ids(sortEnvironments(items, "nodes", "desc"))).toEqual(["1", "4", "3", "2"]);
  });

  it("sorts by failing services and keeps unknown counts last", () => {
    const items = [
      env("1", "a", "online", { failingServices: { failing: 0, total: 5 } }),
      env("2", "b", "online", { failingServices: null }),
      env("3", "c", "online", { failingServices: { failing: 2, total: 5 } }),
    ];
    expect(ids(sortEnvironments(items, "failing", "desc"))).toEqual(["3", "1", "2"]);
    expect(ids(sortEnvironments(items, "failing", "asc"))).toEqual(["1", "3", "2"]);
  });

  it("sorts by last seen, oldest first when ascending, never-seen last", () => {
    const items = [
      env("1", "a", "online", { lastSeen: "2026-10-03T10:05:00Z" }),
      env("2", "b", "online", { lastSeen: null }),
      env("3", "c", "online", { lastSeen: "2026-10-03T10:00:00Z" }),
    ];
    expect(ids(sortEnvironments(items, "lastSeen", "asc"))).toEqual(["3", "1", "2"]);
    expect(ids(sortEnvironments(items, "lastSeen", "desc"))).toEqual(["1", "3", "2"]);
  });

  it("treats a missing direction as ascending", () => {
    const items = [env("2", "b"), env("1", "a")];
    expect(ids(sortEnvironments(items, "name"))).toEqual(["1", "2"]);
  });
});
