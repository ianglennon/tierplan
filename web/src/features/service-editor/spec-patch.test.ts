// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { describe, expect, it } from "vitest";
import { getPath, removePath, setPath } from "@/features/service-editor/spec-patch";
import everyField from "@/features/service-editor/testdata/every-field-spec.json";

type Json = Record<string, unknown>;

// freeze makes a write to the input throw, so a function that mutates fails loudly.
function freeze<T>(v: T): T {
  if (typeof v === "object" && v !== null) {
    for (const k of Object.keys(v)) freeze((v as Json)[k]);
    Object.freeze(v);
  }
  return v;
}

const fresh = (): Json => freeze(structuredClone(everyField) as Json);

describe("getPath", () => {
  it("reads a dotted path", () => {
    expect(getPath(fresh(), "TaskTemplate.ContainerSpec.Image")).toMatch(/^registry\.example\.com/);
  });

  it("reads an index", () => {
    expect(getPath(fresh(), "EndpointSpec.Ports[1].TargetPort")).toBe(53);
    expect(getPath(fresh(), "TaskTemplate.ContainerSpec.Env[0]")).toBe("MODE=prod");
  });

  it("reads an index of an index", () => {
    expect(getPath({ a: [[1, 2], [3]] }, "a[0][1]")).toBe(2);
  });

  it("is undefined for a path that is not there, or that goes through a value that is not a container", () => {
    expect(getPath(fresh(), "TaskTemplate.Nope.Deeper")).toBeUndefined();
    expect(getPath(fresh(), "Name.Deeper")).toBeUndefined();
    expect(getPath({ a: null }, "a.b")).toBeUndefined();
    expect(getPath({ a: [1] }, "a[5]")).toBeUndefined();
  });

  it("returns a null that is there as null", () => {
    expect(getPath({ a: null }, "a")).toBeNull();
  });

  it("returns the whole value for an empty path", () => {
    const spec = fresh();
    expect(getPath(spec, "")).toBe(spec);
  });
});

describe("setPath", () => {
  it("returns a new object with only that value changed", () => {
    const spec = fresh();
    const next = setPath(spec, "TaskTemplate.ContainerSpec.Image", "nginx:1.28") as Json;
    expect(next).not.toBe(spec);
    expect(getPath(next, "TaskTemplate.ContainerSpec.Image")).toBe("nginx:1.28");
    const expected = structuredClone(everyField) as Json;
    ((expected.TaskTemplate as Json).ContainerSpec as Json).Image = "nginx:1.28";
    expect(next).toEqual(expected);
  });

  it("keeps the order of every key, the changed one included", () => {
    const spec = fresh();
    const next = setPath(spec, "TaskTemplate.ContainerSpec.Image", "nginx:1.28");
    const keysOf = (v: unknown) => Object.keys(v as Json);
    expect(keysOf(next)).toEqual(keysOf(spec));
    expect(keysOf(getPath(next, "TaskTemplate"))).toEqual(keysOf(getPath(spec, "TaskTemplate")));
    expect(keysOf(getPath(next, "TaskTemplate.ContainerSpec"))).toEqual(
      keysOf(getPath(spec, "TaskTemplate.ContainerSpec")),
    );
  });

  it("does not touch the input and shares what it did not change", () => {
    const spec = fresh();
    const before = JSON.stringify(spec);
    const next = setPath(spec, "Mode.Replicated.Replicas", 5) as Json;
    expect(JSON.stringify(spec)).toBe(before);
    expect(next.TaskTemplate).toBe(spec.TaskTemplate);
    expect(next.EndpointSpec).toBe(spec.EndpointSpec);
    expect(getPath(next, "Mode.Replicated.Replicas")).toBe(5);
  });

  it("sets one element of an array and leaves the others", () => {
    const next = setPath(fresh(), "EndpointSpec.Ports[1].PublishedPort", 5454);
    expect(getPath(next, "EndpointSpec.Ports[1].PublishedPort")).toBe(5454);
    expect(getPath(next, "EndpointSpec.Ports[0]")).toEqual(
      getPath(fresh(), "EndpointSpec.Ports[0]"),
    );
  });

  it("creates missing parents as objects, and as arrays before an index", () => {
    expect(setPath({}, "TaskTemplate.Resources.Limits.NanoCPUs", 5e8)).toEqual({
      TaskTemplate: { Resources: { Limits: { NanoCPUs: 5e8 } } },
    });
    expect(setPath({}, "EndpointSpec.Ports[0].TargetPort", 80)).toEqual({
      EndpointSpec: { Ports: [{ TargetPort: 80 }] },
    });
  });

  it("replaces a null or a scalar parent with a container", () => {
    expect(setPath({ a: null }, "a.b", 1)).toEqual({ a: { b: 1 } });
    expect(setPath({ a: "x" }, "a.b", 1)).toEqual({ a: { b: 1 } });
  });

  it("appends at the end of an array and refuses to leave a gap", () => {
    expect(setPath({ a: [1] }, "a[1]", 2)).toEqual({ a: [1, 2] });
    expect(() => setPath({ a: [1] }, "a[3]", 2)).toThrow(RangeError);
  });

  it("sets a whole object or array as a value", () => {
    const next = setPath(fresh(), "Labels", { only: "this" });
    expect(getPath(next, "Labels")).toEqual({ only: "this" });
  });

  it("returns the input itself when the value is already there", () => {
    const spec = fresh();
    expect(setPath(spec, "Name", "web")).toBe(spec);
  });

  it("refuses a segment that would reach the prototype", () => {
    expect(() => setPath({}, "__proto__.polluted", 1)).toThrow();
    expect(() => setPath({}, "a.constructor.prototype.x", 1)).toThrow();
    expect(({} as Json).polluted).toBeUndefined();
  });

  it("refuses an empty or malformed path", () => {
    expect(() => setPath({}, "", 1)).toThrow();
    expect(() => setPath({}, "a..b", 1)).toThrow();
    expect(() => setPath({}, "a[x]", 1)).toThrow();
    expect(() => setPath({}, "a[", 1)).toThrow();
  });
});

describe("removePath", () => {
  it("deletes a leaf and prunes nothing else", () => {
    const next = removePath(fresh(), "TaskTemplate.Resources.Limits.NanoCPUs") as Json;
    expect(getPath(next, "TaskTemplate.Resources.Limits")).toEqual({
      MemoryBytes: 8589934592,
      Pids: 4096,
    });
    const only = removePath(
      { TaskTemplate: { Resources: { Limits: { NanoCPUs: 1 } } } },
      "TaskTemplate.Resources.Limits.NanoCPUs",
    );
    expect(only).toEqual({ TaskTemplate: { Resources: { Limits: {} } } });
  });

  it("keeps the order of the keys that stay", () => {
    const next = removePath(fresh(), "TaskTemplate.ContainerSpec.Hostname");
    const keys = Object.keys(getPath(next, "TaskTemplate.ContainerSpec") as Json);
    const was = Object.keys(getPath(fresh(), "TaskTemplate.ContainerSpec") as Json).filter(
      (k) => k !== "Hostname",
    );
    expect(keys).toEqual(was);
  });

  it("takes an element out of an array", () => {
    const next = removePath(fresh(), "EndpointSpec.Ports[0]");
    expect(getPath(next, "EndpointSpec.Ports")).toEqual([
      getPath(fresh(), "EndpointSpec.Ports[1]"),
    ]);
  });

  it("does not touch the input", () => {
    const spec = fresh();
    const before = JSON.stringify(spec);
    removePath(spec, "Labels");
    expect(JSON.stringify(spec)).toBe(before);
  });

  it("returns the input itself for a path that is not there", () => {
    const spec = fresh();
    expect(removePath(spec, "TaskTemplate.Nope.Deeper")).toBe(spec);
    expect(removePath(spec, "Name.Deeper")).toBe(spec);
    expect(removePath(spec, "EndpointSpec.Ports[9]")).toBe(spec);
  });

  it("refuses an empty path", () => {
    expect(() => removePath({}, "")).toThrow();
  });
});

describe("a patch that changes nothing", () => {
  const paths = [
    "Name",
    "Labels",
    "Mode.Replicated.Replicas",
    "TaskTemplate.ContainerSpec.Image",
    "TaskTemplate.ContainerSpec.Env",
    "EndpointSpec.Ports",
    "TaskTemplate.Resources.Limits.NanoCPUs",
    "TaskTemplate.Resources.Limits.MemoryBytes",
    "TaskTemplate.Resources.Reservations.NanoCPUs",
    "TaskTemplate.Resources.Reservations.MemoryBytes",
  ];

  it.each(paths)("setting %s to what it holds leaves every byte of the spec", (path) => {
    const spec = fresh();
    const next = setPath(spec, path, structuredClone(getPath(spec, path)));
    expect(JSON.stringify(next)).toBe(JSON.stringify(spec));
  });
});
