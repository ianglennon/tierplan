// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { describe, expect, it } from "vitest";

import {
  ENVIRONMENT_PERMISSIONS,
  EXTRA_PERMISSIONS,
  GLOBAL_PERMISSIONS,
  PERMISSION_REQUIREMENTS,
} from "@/api/permissions.gen";
import { makeMe } from "@/test-me";
import { splitPermissions } from "@/test-permissions";

describe("makeMe", () => {
  it("gives an instance admin every permission, the one * entry and the registry's requirements", () => {
    const me = makeMe({ instanceAdmin: true });
    expect(me.user.instanceAdmin).toBe(true);
    expect(me.permissions.global).toEqual([...GLOBAL_PERMISSIONS]);
    expect(me.permissions.environments).toEqual({ "*": [...ENVIRONMENT_PERMISSIONS] });
    expect(me.access).toEqual({
      "*": { role: "admin", extras: [...EXTRA_PERMISSIONS], sources: [{ kind: "instance" }] },
    });
    expect(me.requirements).toEqual(PERMISSION_REQUIREMENTS);
    expect(me.administrators).toEqual([]);
  });

  it("gives a viewer on one environment no global permission and only the viewer-level ones there", () => {
    const me = makeMe({ envs: { A: "viewer" } });
    expect(me.user.instanceAdmin).toBe(false);
    expect(me.permissions.global).toEqual([]);
    const list = me.permissions.environments.A ?? [];
    expect(list).toContain("environments.read");
    expect(list).toContain("containers.read");
    expect(list).not.toContain("containers.exec");
    expect(list).not.toContain("environments.configure");
    expect(me.access).toEqual({ A: { role: "viewer", extras: [], sources: [{ kind: "direct" }] } });
    expect(me.administrators).toEqual([]);
  });

  it("gives an operator only the extras it carries, and an admin all of them", () => {
    const op = makeMe({ envs: { A: "operator" }, extras: { A: ["containers.exec"] } });
    const list = op.permissions.environments.A ?? [];
    expect(list).toContain("containers.exec");
    expect(list).not.toContain("secrets.reveal");
    expect(list).not.toContain("environments.configure");
    expect(op.access.A?.extras).toEqual(["containers.exec"]);

    const admin = makeMe({ envs: { A: "admin" } });
    expect(admin.permissions.environments.A).toEqual([...ENVIRONMENT_PERMISSIONS]);
  });

  it("puts an all-environments role under *, labelled as such", () => {
    const me = makeMe({ allEnvironments: "viewer" });
    expect(me.permissions.environments["*"]).toContain("environments.read");
    expect(me.access["*"]?.sources).toEqual([{ kind: "all" }]);
  });

  it("lists the administrators for a user with no access at all", () => {
    expect(makeMe({ envs: {} }).administrators).toEqual(["admin"]);
    expect(makeMe({ envs: {}, administrators: ["ann", "bo"] }).administrators).toEqual(["ann", "bo"]);
  });

  it("takes an exact permission list when a test needs one", () => {
    const perms = { global: ["audit.read"], environments: { "*": ["environments.read"] } };
    expect(makeMe({ permissions: perms }).permissions).toEqual(perms);
  });
});

describe("splitPermissions", () => {
  it("splits on the generated global list, so a permission the old hand-kept list lacked is still global", () => {
    expect(splitPermissions(["users.manage", "containers.read"])).toEqual({
      global: ["users.manage"],
      environments: { "*": ["containers.read"] },
    });
  });
});
