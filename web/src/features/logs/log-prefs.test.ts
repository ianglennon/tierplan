// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { afterEach, describe, expect, it, vi } from "vitest";

import {
  DEFAULT_LOG_PREFS,
  formatShow,
  fromLocalInput,
  loadLogPrefs,
  loadTaskNames,
  logSearchSchema,
  parseShow,
  saveLogPrefs,
  saveTaskNames,
  serviceLogSearchSchema,
  streamRange,
  toLocalInput,
} from "@/features/logs/log-prefs";

// The choices a person makes about how logs read, and the Show range (plan 04-20,
// D-15, D-19, D-22).

afterEach(() => {
  vi.unstubAllGlobals();
  localStorage.clear();
});

describe("log preferences", () => {
  it("start with wrap, timestamps and UTC off", () => {
    expect(loadLogPrefs()).toEqual({ wrap: false, timestamps: false, utc: false });
    expect(DEFAULT_LOG_PREFS).toEqual({ wrap: false, timestamps: false, utc: false });
  });

  it("are remembered per browser, and hold nothing but the three choices", () => {
    saveLogPrefs({ wrap: true, timestamps: true, utc: false });
    expect(loadLogPrefs()).toEqual({ wrap: true, timestamps: true, utc: false });
    const keys = Object.keys(localStorage);
    expect(keys).toEqual(["coaming.logs.view"]);
    expect(JSON.parse(localStorage.getItem("coaming.logs.view") ?? "")).toEqual({
      wrap: true,
      timestamps: true,
      utc: false,
    });
  });

  it("fall back to the defaults when what was saved is damaged", () => {
    localStorage.setItem("coaming.logs.view", "{not json");
    expect(loadLogPrefs()).toEqual(DEFAULT_LOG_PREFS);
    localStorage.setItem("coaming.logs.view", JSON.stringify({ wrap: "yes", utc: 1 }));
    expect(loadLogPrefs()).toEqual(DEFAULT_LOG_PREFS);
  });

  it("do not stop the page when storage is off", () => {
    vi.stubGlobal("localStorage", {
      getItem: () => {
        throw new Error("blocked");
      },
      setItem: () => {
        throw new Error("blocked");
      },
    });
    expect(loadLogPrefs()).toEqual(DEFAULT_LOG_PREFS);
    expect(() => saveLogPrefs({ wrap: true, timestamps: true, utc: true })).not.toThrow();
  });
});

describe("the Logs address", () => {
  it("keeps what fits and drops what does not", () => {
    expect(
      logSearchSchema.parse({ since: "1h", q: "error", ts: true, utc: false, wrap: true }),
    ).toEqual({
      since: "1h",
      q: "error",
      ts: true,
      utc: false,
      wrap: true,
    });
    const odd = logSearchSchema.parse({ since: "", q: 404, ts: "yes", wrap: { a: 1 } });
    expect(odd.q).toBe("404");
    expect(odd.since).toBeUndefined();
    expect(odd.ts).toBeUndefined();
    expect(odd.wrap).toBeUndefined();
    expect(logSearchSchema.parse({ q: "x".repeat(501) }).q).toBeUndefined();
  });
});

describe("the Show range", () => {
  it("is the last 500 lines unless the address says otherwise", () => {
    expect(parseShow(undefined)).toEqual({ kind: "lines" });
    expect(parseShow("nonsense")).toEqual({ kind: "lines" });
    expect(formatShow({ kind: "lines" })).toBeUndefined();
  });

  it("reads and writes the presets and all", () => {
    for (const preset of ["5m", "1h", "24h"] as const) {
      expect(parseShow(preset)).toEqual({ kind: "last", preset });
      expect(formatShow({ kind: "last", preset })).toBe(preset);
    }
    expect(parseShow("all")).toEqual({ kind: "all" });
    expect(formatShow({ kind: "all" })).toBe("all");
  });

  it("reads and writes a custom range with or without an end", () => {
    const from = "2026-10-09T10:00:00.000Z";
    const to = "2026-10-09T11:30:00.000Z";
    expect(parseShow(from)).toEqual({ kind: "custom", from });
    expect(parseShow(`${from}..${to}`)).toEqual({ kind: "custom", from, to });
    expect(formatShow({ kind: "custom", from, to })).toBe(`${from}..${to}`);
    expect(formatShow({ kind: "custom", from })).toBe(from);
    // An end before the start is not a range.
    expect(parseShow(`${to}..${from}`)).toEqual({ kind: "lines" });
  });

  it("asks the stream for the tail, or for a time range with a tail that holds it", () => {
    const now = new Date("2026-10-09T12:00:00.000Z");
    expect(streamRange({ kind: "lines" }, now)).toEqual({ tail: 500 });
    expect(streamRange({ kind: "all" }, now)).toEqual({ tail: "all" });
    expect(streamRange({ kind: "last", preset: "5m" }, now)).toEqual({
      tail: 50_000,
      since: "2026-10-09T11:55:00.000Z",
    });
    expect(streamRange({ kind: "last", preset: "24h" }, now).since).toBe(
      "2026-10-08T12:00:00.000Z",
    );
    expect(
      streamRange(
        { kind: "custom", from: "2026-10-09T10:00:00.000Z", to: "2026-10-09T11:00:00.000Z" },
        now,
      ),
    ).toEqual({
      tail: 50_000,
      since: "2026-10-09T10:00:00.000Z",
      until: "2026-10-09T11:00:00.000Z",
    });
  });

  it("converts to and from the value of a datetime-local input in the browser's zone", () => {
    const iso = "2026-10-09T10:15:00.000Z";
    const local = toLocalInput(iso);
    expect(local).toMatch(/^2026-10-(09|10)T\d\d:\d\d$/);
    expect(fromLocalInput(local)).toBe(iso);
    expect(fromLocalInput("")).toBeUndefined();
    expect(toLocalInput("garbage")).toBe("");
  });
});

// The Task names choice and the address of a service's log (plan 06-29, D-32, D-35).
describe("the Task names choice", () => {
  it("is on until it is turned off", () => {
    expect(loadTaskNames()).toBe(true);
    saveTaskNames(false);
    expect(loadTaskNames()).toBe(false);
    saveTaskNames(true);
    expect(loadTaskNames()).toBe(true);
  });

  it("is kept with the other choices, and adds nothing to what they are read as", () => {
    saveLogPrefs({ wrap: true, timestamps: false, utc: false });
    saveTaskNames(false);
    expect(Object.keys(localStorage)).toEqual(["coaming.logs.view"]);
    expect(loadLogPrefs()).toEqual({ wrap: true, timestamps: false, utc: false });
    expect(JSON.parse(localStorage.getItem("coaming.logs.view") ?? "")).toEqual({
      wrap: true,
      timestamps: false,
      utc: false,
      taskNames: false,
    });
  });

  it("survives the other choices being saved again", () => {
    saveTaskNames(false);
    saveLogPrefs({ wrap: true, timestamps: true, utc: true });
    expect(loadTaskNames()).toBe(false);
    expect(loadLogPrefs()).toEqual({ wrap: true, timestamps: true, utc: true });
  });

  it("is on when what was saved is damaged, and does not stop the page when storage is off", () => {
    localStorage.setItem("coaming.logs.view", "{not json");
    expect(loadTaskNames()).toBe(true);
    localStorage.setItem("coaming.logs.view", JSON.stringify({ taskNames: "no" }));
    expect(loadTaskNames()).toBe(true);
    vi.stubGlobal("localStorage", {
      getItem: () => {
        throw new Error("blocked");
      },
      setItem: () => {
        throw new Error("blocked");
      },
    });
    expect(loadTaskNames()).toBe(true);
    expect(() => saveTaskNames(false)).not.toThrow();
  });
});

describe("the address of a service's log", () => {
  it("keeps the container log's choices and adds the prefix, the tasks, the nodes and the stopped switch", () => {
    expect(
      serviceLogSearchSchema.parse({
        since: "1h",
        q: "x",
        prefix: false,
        task: "t1,t2",
        node: "alpha",
        stopped: true,
      }),
    ).toEqual({ since: "1h", q: "x", prefix: false, task: "t1,t2", node: "alpha", stopped: true });
  });

  it("reads prefix=0 as off, as the router hands it over a number, and drops what does not fit", () => {
    expect(serviceLogSearchSchema.parse({ prefix: 0 }).prefix).toBe(false);
    expect(serviceLogSearchSchema.parse({ prefix: "0" }).prefix).toBe(false);
    expect(serviceLogSearchSchema.parse({ prefix: 1 }).prefix).toBe(true);
    expect(serviceLogSearchSchema.parse({ prefix: "maybe" }).prefix).toBeUndefined();
    expect(serviceLogSearchSchema.parse({ stopped: 1 }).stopped).toBe(true);
    expect(serviceLogSearchSchema.parse({ stopped: "x" }).stopped).toBeUndefined();
    expect(serviceLogSearchSchema.parse({ task: "", node: 7 }).task).toBeUndefined();
    expect(serviceLogSearchSchema.parse({ task: "", node: 7 }).node).toBe("7");
  });
});
