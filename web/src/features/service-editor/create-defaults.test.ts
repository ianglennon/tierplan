// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { describe, expect, it } from "vitest";

import { defaultServiceSpec } from "@/features/service-editor/create-defaults";
import { getPath } from "@/features/service-editor/spec-patch";

// The spec the Create service page opens with (plan 06-27, D-04; UI-SPEC "Create defaults"):
// Replicated, 1 replica, restart on any exit after 5s, update 1 task at a time stop-first with no
// delay, no limits, no networks, secrets, configs or ports, and an empty image that is required.

describe("defaultServiceSpec", () => {
  const spec = defaultServiceSpec();

  it("is replicated with one replica", () => {
    expect(getPath(spec, "Mode")).toEqual({ Replicated: { Replicas: 1 } });
  });

  it("restarts on any exit after 5 seconds", () => {
    expect(getPath(spec, "TaskTemplate.RestartPolicy")).toEqual({
      Condition: "any",
      Delay: 5_000_000_000,
    });
  });

  it("updates one task at a time, stopping each first, with no delay and no automatic rollback", () => {
    expect(getPath(spec, "UpdateConfig")).toMatchObject({
      Parallelism: 1,
      Delay: 0,
      Order: "stop-first",
      FailureAction: "pause",
    });
  });

  it("has an empty name and an empty image, for the person to fill in", () => {
    expect(getPath(spec, "Name")).toBe("");
    expect(getPath(spec, "TaskTemplate.ContainerSpec.Image")).toBe("");
  });

  it("has no limits, networks, secrets, configs or ports", () => {
    expect(getPath(spec, "TaskTemplate.Resources")).toBeUndefined();
    expect(getPath(spec, "TaskTemplate.Networks")).toBeUndefined();
    expect(getPath(spec, "TaskTemplate.ContainerSpec.Secrets")).toBeUndefined();
    expect(getPath(spec, "TaskTemplate.ContainerSpec.Configs")).toBeUndefined();
    expect(getPath(spec, "EndpointSpec")).toBeUndefined();
  });

  it("is a new object each time, so one page's edits never reach the next", () => {
    const a = defaultServiceSpec();
    const b = defaultServiceSpec();
    expect(a).not.toBe(b);
    (a.Mode as { Replicated: { Replicas: number } }).Replicated.Replicas = 9;
    expect(getPath(b, "Mode")).toEqual({ Replicated: { Replicas: 1 } });
  });
});
