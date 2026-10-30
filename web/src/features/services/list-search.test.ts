// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { describe, expect, it } from "vitest";

import {
  SERVICE_STATUS_VALUES,
  serviceSearchSchema,
  serviceStatusList,
  serviceStatusParam,
} from "@/features/services/list-search";

describe("serviceSearchSchema", () => {
  it("keeps a search that fits, the status chips in chip order, group, sort and dir", () => {
    const out = serviceSearchSchema.parse({
      q: "web",
      status: "problem,degraded",
      group: "none",
      sort: "updated",
      dir: "desc",
    });
    expect(out).toEqual({
      q: "web",
      status: "degraded,problem",
      group: "none",
      sort: "updated",
      dir: "desc",
    });
  });

  it("drops each value that does not fit and keeps the rest", () => {
    const out = serviceSearchSchema.parse({
      q: "x".repeat(129),
      status: "sleepy",
      group: "node",
      sort: "created",
      dir: "sideways",
    });
    expect(out).toEqual({
      q: undefined,
      status: undefined,
      group: undefined,
      sort: undefined,
      dir: undefined,
    });
  });

  it("accepts a search of exactly 128 characters and refuses an empty one", () => {
    expect(serviceSearchSchema.parse({ q: "x".repeat(128) }).q).toHaveLength(128);
    expect(serviceSearchSchema.parse({ q: "" }).q).toBeUndefined();
  });

  it("is an empty search for an empty URL", () => {
    expect(serviceSearchSchema.parse({})).toEqual({});
  });
});

describe("the status chips", () => {
  it("are degraded, updating, problem and loop", () => {
    expect(SERVICE_STATUS_VALUES).toEqual(["degraded", "updating", "problem", "loop"]);
  });

  it("keep the loop chip last whatever order the link wrote them in", () => {
    expect(serviceStatusList("loop,degraded")).toEqual(["degraded", "loop"]);
    expect(serviceStatusParam(["loop", "problem"])).toBe("problem,loop");
    expect(serviceSearchSchema.parse({ status: "loop" }).status).toBe("loop");
  });

  it("read a comma list into known values, once each, in chip order, ignoring case and spaces", () => {
    expect(serviceStatusList("problem, UPDATING,problem,nope")).toEqual(["updating", "problem"]);
    expect(serviceStatusList(undefined)).toEqual([]);
    expect(serviceStatusList("")).toEqual([]);
  });

  it("write back as one link, or nothing when none is chosen", () => {
    expect(serviceStatusParam(["problem", "degraded"])).toBe("degraded,problem");
    expect(serviceStatusParam([])).toBeUndefined();
  });
});

describe("the live stats switch", () => {
  it("is kept as stats=1 and nothing else, so the URL says it once", () => {
    expect(serviceSearchSchema.parse({ stats: 1 })).toEqual({ stats: 1 });
    expect(serviceSearchSchema.parse({ stats: "1" })).toEqual({});
    expect(serviceSearchSchema.parse({ stats: 2 })).toEqual({});
    expect(serviceSearchSchema.parse({ stats: true })).toEqual({});
  });
});
