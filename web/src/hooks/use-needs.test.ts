// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { describe, expect, it } from "vitest";

import { needsOf } from "@/hooks/use-needs";
import i18n from "@/i18n";
import { makeMe } from "@/test-me";

const t = (key: string, options?: Record<string, unknown>) =>
  String(i18n.t(key, { ...options, ns: key.split(".")[0] }));

describe("needsOf", () => {
  it("names the extra permission a Viewer lacks for a container shell", () => {
    const me = makeMe({ envs: { e1: "viewer" } });
    expect(needsOf(me, "containers.exec", "e1", t)).toEqual({
      allowed: false,
      reason: "Needs the container shell permission",
    });
  });

  it("names the other extras in the same words", () => {
    const me = makeMe({ envs: { e1: "operator" } });
    expect(needsOf(me, "secrets.reveal", "e1", t).reason).toBe(
      "Needs the reveal secrets permission",
    );
    expect(needsOf(me, "volumes.delete", "e1", t).reason).toBe(
      "Needs the delete volumes permission",
    );
    expect(needsOf(me, "janitor.policy", "e1", t).reason).toBe(
      "Needs the janitor policy permission",
    );
  });

  it("says Needs Admin for an Admin-role permission", () => {
    const me = makeMe({ envs: { e1: "operator" } });
    expect(needsOf(me, "environments.configure", "e1", t)).toEqual({
      allowed: false,
      reason: "Needs Admin",
    });
  });

  it("says Needs Operator for an Operator-role permission, read from the server's requirements", () => {
    const me = makeMe({ envs: { e1: "viewer" } });
    me.requirements["stacks.deploy"] = { kind: "role", role: "operator" };
    expect(needsOf(me, "stacks.deploy", "e1", t).reason).toBe("Needs Operator");
  });

  it("reads the words from the requirement, not from a table of its own", () => {
    const me = makeMe({ envs: { e1: "viewer" } });
    // The server says this one needs Admin; a hard-coded matrix would not know it.
    me.requirements["environments.read"] = { kind: "role", role: "admin" };
    me.permissions.environments.e1 = [];
    expect(needsOf(me, "environments.read", "e1", t).reason).toBe("Needs Admin");
  });

  it("says only an instance admin can do an instance-only thing", () => {
    const me = makeMe({ envs: { e1: "admin" } });
    expect(needsOf(me, "users.manage", undefined, t)).toEqual({
      allowed: false,
      reason: "Only an instance admin can do this",
    });
  });

  it("is allowed, with no reason, for a holder", () => {
    expect(needsOf(makeMe({ instanceAdmin: true }), "users.manage", undefined, t)).toEqual({
      allowed: true,
    });
    expect(needsOf(makeMe({ envs: { e1: "admin" } }), "environments.configure", "e1", t)).toEqual({
      allowed: true,
    });
  });

  it("has no reason for a permission the server does not register, and while the user is not loaded", () => {
    expect(needsOf(makeMe({ envs: { e1: "viewer" } }), "nothing.here", "e1", t)).toEqual({
      allowed: false,
    });
    expect(needsOf(undefined, "users.manage", undefined, t)).toEqual({ allowed: false });
  });

  it("falls back to the permission's own name for an extra it has no words for", () => {
    const me = makeMe({ envs: { e1: "viewer" } });
    me.requirements["registry.push"] = { kind: "extra", role: "operator" };
    expect(needsOf(me, "registry.push", "e1", t).reason).toBe("Needs the registry.push permission");
  });
});
