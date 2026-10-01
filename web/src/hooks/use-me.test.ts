// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { describe, expect, it } from "vitest";

import { hasPermission } from "@/hooks/use-me";

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
