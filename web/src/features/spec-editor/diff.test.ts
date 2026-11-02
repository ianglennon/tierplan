// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { describe, expect, it } from "vitest";

import {
  type DiffRow,
  diffSpecs,
  HIDDEN,
  SPEC_GROUPS,
  specGroupOf,
} from "@/features/spec-editor/diff";

const CS = "TaskTemplate.ContainerSpec";

function spec(container: Record<string, unknown> = {}, rest: Record<string, unknown> = {}) {
  return {
    Name: "web",
    TaskTemplate: { ContainerSpec: { Image: "web:1", ...container } },
    ...rest,
  };
}

const paths = (rows: DiffRow[]) => rows.map((r) => r.path);

describe("diffSpecs", () => {
  it("lists nothing for two equal specs", () => {
    expect(diffSpecs(spec({ Env: ["A=1"] }), spec({ Env: ["A=1"] }))).toEqual([]);
  });

  it("lists a changed image as one Changed row in Basics", () => {
    const rows = diffSpecs(
      spec({ Image: "nginx:1.27@sha256:aaa" }),
      spec({ Image: "nginx:1.28@sha256:bbb" }),
    );
    expect(rows).toEqual([
      {
        path: `${CS}.Image`,
        group: "basics",
        change: "changed",
        current: "nginx:1.27@sha256:aaa",
        next: "nginx:1.28@sha256:bbb",
        masked: false,
        replaced: false,
      },
    ]);
  });

  it("compares environment variables by name, so order does not matter", () => {
    expect(diffSpecs(spec({ Env: ["A=1", "B=2"] }), spec({ Env: ["B=2", "A=1"] }))).toEqual([]);
    const rows = diffSpecs(spec({ Env: ["A=1", "B=2"] }), spec({ Env: ["B=3", "C=4"] }));
    expect(rows.map((r) => [r.path, r.change, r.current, r.next])).toEqual([
      [`${CS}.Env[A]`, "removed", "1", null],
      [`${CS}.Env[B]`, "changed", "2", "3"],
      [`${CS}.Env[C]`, "added", null, "4"],
    ]);
    expect(new Set(rows.map((r) => r.group))).toEqual(new Set(["environment"]));
  });

  it("reads a variable with no value as an empty one", () => {
    const rows = diffSpecs(spec({ Env: ["FLAG"] }), spec({ Env: ["FLAG=on"] }));
    expect(rows.map((r) => [r.path, r.current, r.next])).toEqual([[`${CS}.Env[FLAG]`, "", "on"]]);
  });

  it("compares mounts by target and lists the field that changed", () => {
    const a = [
      { Type: "volume", Source: "data", Target: "/data" },
      { Type: "bind", Source: "/srv", Target: "/etc/conf", ReadOnly: true },
    ];
    expect(diffSpecs(spec({ Mounts: a }), spec({ Mounts: [a[1], a[0]] }))).toEqual([]);
    const rows = diffSpecs(
      spec({ Mounts: a }),
      spec({ Mounts: [{ Type: "volume", Source: "data2", Target: "/data" }, a[1]] }),
    );
    expect(rows.map((r) => [r.path, r.group, r.change, r.current, r.next])).toEqual([
      [`${CS}.Mounts[/data].Source`, "mounts", "changed", "data", "data2"],
    ]);
  });

  it("compares published ports by target port and protocol", () => {
    const base = { Protocol: "tcp", TargetPort: 80, PublishedPort: 8080 };
    const rows = diffSpecs(
      { Name: "web", EndpointSpec: { Ports: [base] } },
      {
        Name: "web",
        EndpointSpec: {
          Ports: [
            { ...base, PublishedPort: 9090 },
            { Protocol: "udp", TargetPort: 53, PublishedPort: 53 },
          ],
        },
      },
    );
    expect(rows.map((r) => [r.path, r.group, r.change, r.current, r.next])).toEqual([
      ["EndpointSpec.Ports[80/tcp].PublishedPort", "ports", "changed", "8080", "9090"],
      ["EndpointSpec.Ports[53/udp].Protocol", "ports", "added", null, "udp"],
      ["EndpointSpec.Ports[53/udp].TargetPort", "ports", "added", null, "53"],
      ["EndpointSpec.Ports[53/udp].PublishedPort", "ports", "added", null, "53"],
    ]);
  });

  it("compares secrets and configs by file name and networks by target", () => {
    const sec = (id: string) => ({
      SecretID: id,
      SecretName: "db",
      File: { Name: "db_pw", UID: "0" },
    });
    const cfg = (id: string) => ({
      ConfigID: id,
      ConfigName: "app",
      File: { Name: "/etc/app.conf" },
    });
    const rows = diffSpecs(
      spec({ Secrets: [sec("s1")], Configs: [cfg("c1")] }),
      spec({ Secrets: [sec("s2")], Configs: [cfg("c2")] }),
    );
    expect(rows.map((r) => r.path)).toEqual([
      `${CS}.Secrets[db_pw].SecretID`,
      `${CS}.Configs[/etc/app.conf].ConfigID`,
    ]);
    expect(rows.map((r) => r.group)).toEqual(["secrets", "configs"]);

    const nets = diffSpecs(
      { TaskTemplate: { Networks: [{ Target: "front" }, { Target: "back" }] } },
      { TaskTemplate: { Networks: [{ Target: "back" }, { Target: "side", Aliases: ["a"] }] } },
    );
    expect(nets.map((r) => [r.path, r.group, r.change])).toEqual([
      ["TaskTemplate.Networks[front].Target", "networks", "removed"],
      ["TaskTemplate.Networks[side].Target", "networks", "added"],
      ["TaskTemplate.Networks[side].Aliases[0]", "networks", "added"],
    ]);
  });

  it("compares other lists by index", () => {
    const rows = diffSpecs(
      { TaskTemplate: { Placement: { Constraints: ["a", "b"] } } },
      { TaskTemplate: { Placement: { Constraints: ["a", "c", "d"] } } },
    );
    expect(rows.map((r) => [r.path, r.group, r.change, r.current, r.next])).toEqual([
      ["TaskTemplate.Placement.Constraints[1]", "placement", "changed", "b", "c"],
      ["TaskTemplate.Placement.Constraints[2]", "placement", "added", null, "d"],
    ]);
  });

  it("lists the leaves of a whole section that was added or removed", () => {
    const rows = diffSpecs(
      { Name: "web" },
      { Name: "web", UpdateConfig: { Parallelism: 2, FailureAction: "rollback" } },
    );
    expect(rows.map((r) => [r.path, r.group, r.change, r.next])).toEqual([
      ["UpdateConfig.Parallelism", "updatePolicy", "added", "2"],
      ["UpdateConfig.FailureAction", "updatePolicy", "added", "rollback"],
    ]);
    const gone = diffSpecs({ Name: "web", UpdateConfig: { Parallelism: 2 } }, { Name: "web" });
    expect(gone.map((r) => [r.path, r.change, r.current, r.next])).toEqual([
      ["UpdateConfig.Parallelism", "removed", "2", null],
    ]);
  });

  it("reads numbers, booleans and the absence of an empty section", () => {
    const rows = diffSpecs(
      { Name: "web", TaskTemplate: { Resources: {}, ForceUpdate: 1, Runtime: null } },
      {
        Name: "web",
        TaskTemplate: {
          Resources: { Limits: { MemoryBytes: 1024 } },
          ForceUpdate: 2,
          ContainerSpec: { TTY: true },
        },
      },
    );
    expect(rows.map((r) => [r.path, r.change, r.current, r.next])).toEqual([
      ["TaskTemplate.Resources.Limits.MemoryBytes", "added", null, "1024"],
      ["TaskTemplate.ForceUpdate", "changed", "1", "2"],
      ["TaskTemplate.ContainerSpec.TTY", "added", null, "true"],
    ]);
  });

  it("treats a missing side as an empty spec", () => {
    const rows = diffSpecs(undefined, { Name: "web" });
    expect(rows.map((r) => [r.path, r.change, r.next])).toEqual([["Name", "added", "web"]]);
    expect(diffSpecs(null, null)).toEqual([]);
  });

  it("orders the rows by group, then as the spec lists them", () => {
    const rows = diffSpecs(
      {
        Name: "a",
        UpdateConfig: { Parallelism: 1 },
        TaskTemplate: {
          ContainerSpec: { Env: ["X=1"], Image: "i:1" },
          Placement: { MaxReplicas: 1 },
        },
      },
      {
        Name: "b",
        UpdateConfig: { Parallelism: 2 },
        TaskTemplate: {
          ContainerSpec: { Env: ["X=2"], Image: "i:2" },
          Placement: { MaxReplicas: 2 },
        },
      },
    );
    expect(rows.map((r) => r.group)).toEqual([
      "basics",
      "basics",
      "environment",
      "placement",
      "updatePolicy",
    ]);
    expect(paths(rows)).toEqual([
      "Name",
      `${CS}.Image`,
      `${CS}.Env[X]`,
      "TaskTemplate.Placement.MaxReplicas",
      "UpdateConfig.Parallelism",
    ]);
  });
});

describe("diffSpecs with withheld values", () => {
  it("does not list a withheld value that is withheld on both sides", () => {
    const withheld = spec({
      Env: [`DB_PASSWORD=${HIDDEN}`, "MODE=prod"],
      Command: [HIDDEN],
      Healthcheck: { Test: ["CMD-SHELL", HIDDEN] },
    });
    expect(diffSpecs(withheld, withheld)).toEqual([]);
    const rows = diffSpecs(
      withheld,
      spec({
        Env: [`DB_PASSWORD=${HIDDEN}`, "MODE=dev"],
        Command: [HIDDEN],
        Healthcheck: { Test: ["CMD-SHELL", HIDDEN] },
      }),
    );
    expect(rows.map((r) => [r.path, r.masked])).toEqual([[`${CS}.Env[MODE]`, false]]);
  });

  it("marks a withheld value that was replaced, and carries neither real value", () => {
    const rows = diffSpecs(
      spec({ Env: [`DB_PASSWORD=${HIDDEN}`] }),
      spec({ Env: ["DB_PASSWORD=hunter2-new"] }),
    );
    expect(rows).toHaveLength(1);
    expect(rows[0]).toMatchObject({
      path: `${CS}.Env[DB_PASSWORD]`,
      change: "changed",
      masked: true,
      replaced: true,
      current: HIDDEN,
      next: HIDDEN,
    });
    expect(JSON.stringify(rows)).not.toContain("hunter2");
    const back = diffSpecs(
      spec({ Env: ["DB_PASSWORD=hunter2-old"] }),
      spec({ Env: [`DB_PASSWORD=${HIDDEN}`] }),
    );
    expect(back[0]).toMatchObject({ masked: true, replaced: true, current: HIDDEN, next: HIDDEN });
    expect(JSON.stringify(back)).not.toContain("hunter2");
  });

  it("marks a withheld variable that was added or removed as masked without its value", () => {
    const rows = diffSpecs(spec({ Env: [] }), spec({ Env: [`TOKEN=${HIDDEN}`] }));
    expect(rows).toEqual([
      {
        path: `${CS}.Env[TOKEN]`,
        group: "environment",
        change: "added",
        current: null,
        next: HIDDEN,
        masked: true,
        replaced: false,
      },
    ]);
  });

  it("handles a withheld log driver option and a withheld scalar", () => {
    const a = { Name: "w", TaskTemplate: { LogDriver: { Options: { token: HIDDEN, mode: "x" } } } };
    const b = {
      Name: "w",
      TaskTemplate: { LogDriver: { Options: { token: "real-token", mode: "x" } } },
    };
    const rows = diffSpecs(a, b);
    expect(rows.map((r) => [r.path, r.group, r.masked, r.replaced])).toEqual([
      ["TaskTemplate.LogDriver.Options.token", "other", true, true],
    ]);
    expect(JSON.stringify(rows)).not.toContain("real-token");
  });
});

describe("specGroupOf", () => {
  it.each([
    ["Name", "basics"],
    ["Labels.tier", "basics"],
    ["Mode.Replicated.Replicas", "basics"],
    [`${CS}.Image`, "basics"],
    [`${CS}.Env[A]`, "environment"],
    ["EndpointSpec.Ports[80/tcp].PublishedPort", "ports"],
    ["EndpointSpec.Mode", "ports"],
    [`${CS}.Secrets[x].SecretID`, "secrets"],
    [`${CS}.Configs[x].ConfigID`, "configs"],
    ["TaskTemplate.Networks[front].Target", "networks"],
    ["Networks[front].Target", "networks"],
    [`${CS}.Mounts[/data].Source`, "mounts"],
    ["TaskTemplate.Resources.Limits.NanoCPUs", "resources"],
    ["TaskTemplate.Placement.Constraints[0]", "placement"],
    ["TaskTemplate.RestartPolicy.Condition", "restartPolicy"],
    ["UpdateConfig.Parallelism", "updatePolicy"],
    ["RollbackConfig.Parallelism", "updatePolicy"],
    [`${CS}.Command[0]`, "other"],
    [`${CS}.Healthcheck.Test[1]`, "other"],
    ["TaskTemplate.LogDriver.Name", "other"],
    ["TaskTemplate.ForceUpdate", "other"],
  ])("puts %s under %s", (path, group) => {
    expect(specGroupOf(path)).toBe(group);
  });

  it("lists the groups in the order the cards are shown", () => {
    expect(SPEC_GROUPS[0]).toBe("basics");
    expect(SPEC_GROUPS.at(-1)).toBe("other");
    expect(new Set(SPEC_GROUPS).size).toBe(SPEC_GROUPS.length);
  });
});
