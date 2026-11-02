// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { describe, expect, it } from "vitest";

import type { EventDTO } from "@/features/dashboard/events";
import {
  applyEventFilters,
  DEFAULT_RANGE,
  eventsSearchSchema,
  isGone,
  rangeStart,
} from "@/features/events/filters";

function ev(id: string, over: Partial<EventDTO> = {}): EventDTO {
  return {
    id,
    node: "n1",
    time: "2026-10-02T10:00:00Z",
    type: "container",
    action: "start",
    actorId: `actor-${id}`,
    name: `name-${id}`,
    attributes: {},
    ...over,
  };
}

const EVENTS: EventDTO[] = [
  ev("1", { name: "web", attributes: { image: "nginx:1.27" } }),
  ev("2", { name: "db", action: "die", attributes: { image: "postgres:16", exitCode: "137" } }),
  ev("3", {
    type: "network",
    name: "app-net",
    action: "connect",
    node: "n2",
    attributes: { container: "WEB-1" },
  }),
  ev("4", { type: "volume", name: "data", action: "create", node: "n2" }),
  ev("5", { type: "tierplan", name: "Manager moved to mgr-2", action: "", node: "mgr-2" }),
];

const ids = (list: EventDTO[]) => list.map((e) => e.id);

describe("applyEventFilters", () => {
  it("keeps everything when no filter is set", () => {
    expect(ids(applyEventFilters(EVENTS, {}))).toEqual(["1", "2", "3", "4", "5"]);
    expect(ids(applyEventFilters(EVENTS, { types: [], actions: [] }))).toEqual([
      "1",
      "2",
      "3",
      "4",
      "5",
    ]);
  });

  it("keeps the chosen types, and a row of TierPlan's own only while none is chosen", () => {
    expect(ids(applyEventFilters(EVENTS, { types: ["container"] }))).toEqual(["1", "2"]);
    expect(ids(applyEventFilters(EVENTS, { types: ["network", "volume"] }))).toEqual(["3", "4"]);
  });

  it("keeps the chosen actions and the chosen node", () => {
    expect(ids(applyEventFilters(EVENTS, { actions: ["die", "create"] }))).toEqual(["2", "4"]);
    expect(ids(applyEventFilters(EVENTS, { node: "n2" }))).toEqual(["3", "4"]);
  });

  it("matches the name exactly for the name chip", () => {
    expect(ids(applyEventFilters(EVENTS, { name: "web" }))).toEqual(["1"]);
    expect(ids(applyEventFilters(EVENTS, { name: "we" }))).toEqual([]);
  });

  it("searches name, image and attribute values, ignoring case", () => {
    expect(ids(applyEventFilters(EVENTS, { q: "WEB" }))).toEqual(["1", "3"]);
    expect(ids(applyEventFilters(EVENTS, { q: "postgres" }))).toEqual(["2"]);
    expect(ids(applyEventFilters(EVENTS, { q: "137" }))).toEqual(["2"]);
    expect(ids(applyEventFilters(EVENTS, { q: "  nginx " }))).toEqual(["1"]);
    expect(ids(applyEventFilters(EVENTS, { q: "nothing here" }))).toEqual([]);
  });

  it("does not search attribute names, the action or the node", () => {
    expect(ids(applyEventFilters(EVENTS, { q: "exitCode" }))).toEqual([]);
    expect(ids(applyEventFilters(EVENTS, { q: "connect" }))).toEqual([]);
  });

  it("keeps only events that match every filter that is set, and leaves its input alone", () => {
    const input = [...EVENTS];
    const out = applyEventFilters(input, {
      types: ["container"],
      actions: ["die"],
      q: "postgres",
      node: "n1",
    });
    expect(ids(out)).toEqual(["2"]);
    expect(applyEventFilters(input, { types: ["container"], node: "n2" })).toEqual([]);
    expect(input).toEqual(EVENTS);
  });
});

describe("isGone", () => {
  it("is true for the actions after which the resource no longer exists", () => {
    for (const a of ["destroy", "remove", "delete"]) expect(isGone(a)).toBe(true);
    for (const a of ["start", "die", "create", "connect", "pull"]) expect(isGone(a)).toBe(false);
  });
});

describe("eventsSearchSchema", () => {
  it("reads a good search and drops every part that does not fit", () => {
    expect(
      eventsSearchSchema.parse({
        since: "24h",
        type: ["container", "volume"],
        action: ["die"],
        node: "n1",
        q: "x",
        name: "web",
      }),
    ).toEqual({
      since: "24h",
      type: ["container", "volume"],
      action: ["die"],
      node: "n1",
      q: "x",
      name: "web",
    });
    const bad = eventsSearchSchema.parse({
      since: "3d",
      type: ["container", "tierplan"],
      action: [""],
      node: "",
      q: "x".repeat(500),
      name: 5,
    });
    expect(bad).toEqual({});
  });
});

describe("rangeStart", () => {
  it("reaches back by the range, and the default is an hour", () => {
    const now = Date.parse("2026-10-02T12:00:00Z");
    expect(DEFAULT_RANGE).toBe("1h");
    expect(rangeStart("5m", now)).toBe("2026-10-02T11:55:00.000Z");
    expect(rangeStart("1h", now)).toBe("2026-10-02T11:00:00.000Z");
    expect(rangeStart("24h", now)).toBe("2026-10-01T12:00:00.000Z");
  });
});
