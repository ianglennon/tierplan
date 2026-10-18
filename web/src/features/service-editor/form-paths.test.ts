// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { describe, expect, it } from "vitest";

import { FORM_OWNED_PATHS, isFormOwned, unshownPaths } from "@/features/service-editor/form-paths";
import { getPath, setPath } from "@/features/service-editor/spec-patch";
import everyField from "@/features/service-editor/testdata/every-field-spec.json";

describe("FORM_OWNED_PATHS", () => {
  it("names the paths of the Basics, Environment, Ports, Secrets, Configs, Networks, Mounts, Resources, Placement, Restart policy and Update policy cards", () => {
    expect([...FORM_OWNED_PATHS].sort()).toEqual(
      [
        "EndpointSpec.Ports",
        "Labels",
        "Mode.Global",
        "Mode.Replicated.Replicas",
        "Name",
        "TaskTemplate.ContainerSpec.Configs",
        "TaskTemplate.ContainerSpec.Env",
        "TaskTemplate.ContainerSpec.Image",
        "TaskTemplate.ContainerSpec.Mounts",
        "TaskTemplate.ContainerSpec.Secrets",
        "TaskTemplate.Networks",
        "TaskTemplate.Placement.Constraints",
        "TaskTemplate.Placement.MaxReplicas",
        "TaskTemplate.Placement.Preferences",
        "TaskTemplate.Resources.Limits.MemoryBytes",
        "TaskTemplate.Resources.Limits.NanoCPUs",
        "TaskTemplate.Resources.Reservations.MemoryBytes",
        "TaskTemplate.Resources.Reservations.NanoCPUs",
        "TaskTemplate.RestartPolicy.Condition",
        "TaskTemplate.RestartPolicy.Delay",
        "TaskTemplate.RestartPolicy.MaxAttempts",
        "TaskTemplate.RestartPolicy.Window",
        "UpdateConfig.Delay",
        "UpdateConfig.FailureAction",
        "UpdateConfig.MaxFailureRatio",
        "UpdateConfig.Monitor",
        "UpdateConfig.Order",
        "UpdateConfig.Parallelism",
      ].sort(),
    );
  });
});

describe("isFormOwned", () => {
  it("is true for an owned path and for anything under one", () => {
    expect(isFormOwned("Name")).toBe(true);
    expect(isFormOwned("Labels.com.example.version")).toBe(true);
    expect(isFormOwned("EndpointSpec.Ports")).toBe(true);
    expect(isFormOwned("EndpointSpec.Ports[1].TargetPort")).toBe(true);
    expect(isFormOwned("TaskTemplate.ContainerSpec.Env[0]")).toBe(true);
  });

  it("is false for a path beside an owned one, above one, or that only starts with its text", () => {
    expect(isFormOwned("TaskTemplate.ContainerSpec.Command")).toBe(false);
    expect(isFormOwned("TaskTemplate.ContainerSpec.Labels")).toBe(false);
    expect(isFormOwned("TaskTemplate.Resources.Limits.Pids")).toBe(false);
    expect(isFormOwned("TaskTemplate.Resources")).toBe(false);
    expect(isFormOwned("EndpointSpec")).toBe(false);
    expect(isFormOwned("EndpointSpec.Mode")).toBe(false);
    expect(isFormOwned("NameOfSomethingElse")).toBe(false);
    expect(isFormOwned("TaskTemplate.ContainerSpec.Environment")).toBe(false);
  });
});

describe("unshownPaths", () => {
  it("lists every populated setting no form control owns, shallowest first-found in document order", () => {
    expect(unshownPaths(everyField)).toEqual([
      "TaskTemplate.ContainerSpec.Labels",
      "TaskTemplate.ContainerSpec.Command",
      "TaskTemplate.ContainerSpec.Args",
      "TaskTemplate.ContainerSpec.Hostname",
      "TaskTemplate.ContainerSpec.Dir",
      "TaskTemplate.ContainerSpec.User",
      "TaskTemplate.ContainerSpec.Groups",
      "TaskTemplate.ContainerSpec.Privileges",
      "TaskTemplate.ContainerSpec.Init",
      "TaskTemplate.ContainerSpec.StopSignal",
      "TaskTemplate.ContainerSpec.TTY",
      "TaskTemplate.ContainerSpec.OpenStdin",
      "TaskTemplate.ContainerSpec.ReadOnly",
      "TaskTemplate.ContainerSpec.StopGracePeriod",
      "TaskTemplate.ContainerSpec.Healthcheck",
      "TaskTemplate.ContainerSpec.Hosts",
      "TaskTemplate.ContainerSpec.DNSConfig",
      "TaskTemplate.ContainerSpec.Isolation",
      "TaskTemplate.ContainerSpec.Sysctls",
      "TaskTemplate.ContainerSpec.CapabilityAdd",
      "TaskTemplate.ContainerSpec.CapabilityDrop",
      "TaskTemplate.ContainerSpec.Ulimits",
      "TaskTemplate.ContainerSpec.OomScoreAdj",
      "TaskTemplate.Resources.Limits.Pids",
      "TaskTemplate.Resources.Reservations.GenericResources",
      "TaskTemplate.Placement.Platforms",
      "TaskTemplate.LogDriver",
      "TaskTemplate.ForceUpdate",
      "TaskTemplate.Runtime",
      "RollbackConfig",
      "Networks",
      "EndpointSpec.Mode",
      "Extras",
    ]);
  });

  it("no longer lists the secret, config, network, mount, placement, restart policy and update policy settings the cards own", () => {
    const paths = unshownPaths(everyField);
    for (const p of [
      "TaskTemplate.ContainerSpec.Secrets",
      "TaskTemplate.ContainerSpec.Configs",
      "TaskTemplate.Networks",
      "TaskTemplate.ContainerSpec.Mounts",
      "TaskTemplate.Placement",
      "TaskTemplate.Placement.Constraints",
      "TaskTemplate.Placement.Preferences",
      "TaskTemplate.Placement.MaxReplicas",
      "TaskTemplate.RestartPolicy",
      "UpdateConfig",
    ]) {
      expect(paths, p).not.toContain(p);
    }
  });

  it("still names what the placement, rollback and platform cards do not show", () => {
    const paths = unshownPaths(everyField);
    expect(paths).toContain("TaskTemplate.Placement.Platforms");
    expect(paths).toContain("RollbackConfig");
  });

  it("names a setting beside the owned ones of an update policy", () => {
    expect(unshownPaths({ UpdateConfig: { Parallelism: 2, Unknown: 1 } })).toEqual([
      "UpdateConfig.Unknown",
    ]);
  });

  it("names the command, the health check and the log driver of a spec that has them", () => {
    const paths = unshownPaths(everyField);
    expect(paths).toContain("TaskTemplate.ContainerSpec.Command");
    expect(paths).toContain("TaskTemplate.ContainerSpec.Healthcheck");
    expect(paths).toContain("TaskTemplate.LogDriver");
  });

  it("lists nothing that is, or is inside, a path the form owns", () => {
    for (const p of unshownPaths(everyField)) {
      expect(isFormOwned(p), p).toBe(false);
      for (const owned of FORM_OWNED_PATHS) {
        expect(p === owned || p.startsWith(`${owned}.`) || p.startsWith(`${owned}[`), p).toBe(
          false,
        );
      }
    }
  });

  it("lists nothing for a spec that holds only what the form shows", () => {
    expect(
      unshownPaths({
        Name: "web",
        Labels: { a: "b" },
        TaskTemplate: {
          ContainerSpec: { Image: "nginx:1", Env: ["A=1"] },
          Resources: {
            Limits: { NanoCPUs: 5e8, MemoryBytes: 1 },
            Reservations: { NanoCPUs: 1, MemoryBytes: 1 },
          },
        },
        Mode: { Replicated: { Replicas: 2 } },
        EndpointSpec: { Ports: [{ TargetPort: 80, PublishedPort: 8080 }] },
      }),
    ).toEqual([]);
  });

  it("counts an empty object, an empty array, a null and an empty string as unpopulated", () => {
    expect(
      unshownPaths({
        Name: "web",
        TaskTemplate: {
          ContainerSpec: {
            Command: [],
            Healthcheck: {},
            Dir: null,
            User: "",
            Privileges: { CredentialSpec: { File: "" }, SELinuxContext: {} },
          },
          Placement: { Constraints: [], Preferences: [] },
          LogDriver: {},
        },
        Mode: { Global: {} },
        UpdateConfig: {},
        Networks: [],
        EndpointSpec: {},
      }),
    ).toEqual([]);
  });

  it("counts false and 0 as populated, because the spec states them", () => {
    expect(
      unshownPaths({ TaskTemplate: { ContainerSpec: { Init: false, OomScoreAdj: 0 } } }),
    ).toEqual(["TaskTemplate.ContainerSpec.Init", "TaskTemplate.ContainerSpec.OomScoreAdj"]);
  });

  it("names a job mode, which the form does not edit", () => {
    expect(unshownPaths({ Mode: { ReplicatedJob: { MaxConcurrent: 1 } } })).toEqual([
      "Mode.ReplicatedJob",
    ]);
  });

  it("goes down only as far as it must: a populated setting is named once", () => {
    expect(
      unshownPaths({
        TaskTemplate: { ContainerSpec: { Healthcheck: { Test: ["CMD"], Retries: 3 } } },
      }),
    ).toEqual(["TaskTemplate.ContainerSpec.Healthcheck"]);
  });

  it("lists nothing for something that is not a spec", () => {
    expect(unshownPaths(null)).toEqual([]);
    expect(unshownPaths("x")).toEqual([]);
    expect(unshownPaths([])).toEqual([]);
    expect(unshownPaths({})).toEqual([]);
  });
});

describe("the form's patches over every field", () => {
  it("changing an owned setting leaves the unshown list as it was", () => {
    const before = unshownPaths(everyField);
    let next: unknown = everyField;
    next = setPath(next, "TaskTemplate.ContainerSpec.Image", "nginx:1.28");
    next = setPath(next, "Mode.Replicated.Replicas", 9);
    next = setPath(next, "TaskTemplate.Resources.Limits.NanoCPUs", 1e9);
    expect(unshownPaths(next)).toEqual(before);
    for (const p of before) {
      expect(getPath(next, p)).toEqual(getPath(everyField, p));
    }
  });

  it("round-trips every owned path of the fixture to a deep-equal spec", () => {
    let next: unknown = everyField;
    for (const p of FORM_OWNED_PATHS) {
      const v = getPath(everyField, p);
      if (v !== undefined) next = setPath(next, p, structuredClone(v));
    }
    expect(next).toEqual(everyField);
    expect(JSON.stringify(next)).toBe(JSON.stringify(everyField));
  });
});
