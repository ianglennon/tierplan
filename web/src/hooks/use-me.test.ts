// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { describe, expect, it } from "vitest";

import { hasAnyEnvironmentPermission, hasPermission } from "@/hooks/use-me";

const me = {
  user: { id: "u1", username: "admin", role: "admin" },
  csrfToken: "t",
  permissions: {
    global: ["audit.read", "users.manage"],
    environments: { "*": ["environments.read"], env1: ["containers.read"], env2: null },
  },
};

describe("hasPermission", () => {
  it("is true for a global permission", () => {
    expect(hasPermission(me, "audit.read")).toBe(true);
  });

  it("is false for a global permission the user lacks", () => {
    expect(hasPermission(me, "settings.write")).toBe(false);
  });

  it("is true when the environment list holds the permission", () => {
    expect(hasPermission(me, "containers.read", "env1")).toBe(true);
  });

  it("is true when the * list holds it, for any environment", () => {
    expect(hasPermission(me, "environments.read", "env1")).toBe(true);
    expect(hasPermission(me, "environments.read", "envX")).toBe(true);
  });

  it("is false for an environment that lacks it, and for a null list", () => {
    expect(hasPermission(me, "containers.read", "env2")).toBe(false);
    expect(hasPermission(me, "containers.read", "envX")).toBe(false);
  });

  it("does not let a global permission stand in for an environment one", () => {
    expect(hasPermission(me, "audit.read", "env1")).toBe(false);
  });

  it("is false while the user is not loaded", () => {
    expect(hasPermission(undefined, "audit.read")).toBe(false);
  });
});

describe("hasAnyEnvironmentPermission", () => {
  it("is true when the * list holds the permission", () => {
    expect(hasAnyEnvironmentPermission(me, "environments.read")).toBe(true);
  });

  it("is true when one environment's own list holds it", () => {
    expect(hasAnyEnvironmentPermission(me, "containers.read")).toBe(true);
  });

  it("is false when no environment list holds it, even if the global list does", () => {
    expect(hasAnyEnvironmentPermission(me, "audit.read")).toBe(false);
    expect(hasAnyEnvironmentPermission(me, "volumes.read")).toBe(false);
  });

  it("is false for an empty map, a null list and a user who is not loaded", () => {
    const empty = { ...me, permissions: { global: [], environments: {} } };
    expect(hasAnyEnvironmentPermission(empty, "environments.read")).toBe(false);
    expect(
      hasAnyEnvironmentPermission(
        { ...me, permissions: { global: [], environments: { env2: null } } },
        "environments.read",
      ),
    ).toBe(false);
    expect(hasAnyEnvironmentPermission(undefined, "environments.read")).toBe(false);
  });
});
