// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { afterEach, describe, expect, it, vi } from "vitest";

import {
  POLL_INTERVAL_MS,
  POLL_TIMEOUT_MS,
  sessionProbes,
  setupProbes,
  waitForRestoreOutcome,
} from "@/features/backup/restore-outcome";

// A clock the test owns: sleeping moves it on, so no real time passes.
function clock() {
  let t = 0;
  const sleeps: number[] = [];
  return {
    now: () => t,
    sleep: async (ms: number) => {
      sleeps.push(ms);
      t += ms;
    },
    sleeps,
  };
}

// A server that is up or down per poll, and answers the session check with the
// next status in the list.
function server(script: ("down" | number)[]) {
  let i = 0;
  const calls = { version: 0, me: 0 };
  return {
    calls,
    fetchVersion: async () => {
      const step = script[Math.min(i, script.length - 1)];
      calls.version += 1;
      if (step === "down") {
        i += 1;
        throw new Error("down");
      }
    },
    fetchMe: async () => {
      const step = script[Math.min(i, script.length - 1)];
      calls.me += 1;
      i += 1;
      if (typeof step !== "number") throw new Error("down");
      return step;
    },
  };
}

afterEach(() => vi.unstubAllGlobals());

describe("waitForRestoreOutcome", () => {
  it("is restored when Bayplan went down and came back refusing the session", async () => {
    const c = clock();
    const s = server(["down", "down", 401]);
    await expect(waitForRestoreOutcome({ ...s, ...c })).resolves.toBe("restored");
    expect(c.sleeps).toEqual([POLL_INTERVAL_MS, POLL_INTERVAL_MS]);
  });

  it("is rolled_back when Bayplan went down and came back still accepting the session", async () => {
    const c = clock();
    const s = server(["down", "down", 200]);
    await expect(waitForRestoreOutcome({ ...s, ...c })).resolves.toBe("rolled_back");
  });

  it("does not read an answer from the server that has not stopped yet as a rollback", async () => {
    const c = clock();
    // Still the old server for two polls (200), then down, then back with 200.
    const s = server([200, 200, "down", 200]);
    await expect(waitForRestoreOutcome({ ...s, ...c })).resolves.toBe("rolled_back");
    expect(s.calls.version).toBeGreaterThanOrEqual(4);
  });

  it("is restored on a 401 even if no failed poll was seen, because the old server never refuses the session", async () => {
    const c = clock();
    const s = server([401]);
    await expect(waitForRestoreOutcome({ ...s, ...c })).resolves.toBe("restored");
  });

  it("keeps waiting through a 502 or 503 while Bayplan starts", async () => {
    const c = clock();
    const s = server(["down", 502, 503, 401]);
    await expect(waitForRestoreOutcome({ ...s, ...c })).resolves.toBe("restored");
  });

  it("gives up with timeout after two minutes of nothing", async () => {
    const c = clock();
    const s = server(["down"]);
    await expect(waitForRestoreOutcome({ ...s, ...c })).resolves.toBe("timeout");
    expect(c.now()).toBeGreaterThanOrEqual(POLL_TIMEOUT_MS);
    expect(c.sleeps.every((ms) => ms === POLL_INTERVAL_MS)).toBe(true);
  });

  it("treats a session check that fails after the version answered as the server still being down", async () => {
    const c = clock();
    let n = 0;
    const result = await waitForRestoreOutcome({
      ...c,
      fetchVersion: async () => {},
      fetchMe: async () => {
        n += 1;
        if (n === 1) throw new Error("reset");
        return 200;
      },
    });
    expect(result).toBe("rolled_back");
  });
});

describe("the live probes", () => {
  it("ask /api/version and /api/me without cache, around the typed client", async () => {
    const fetchMock = vi.fn(async (_url: string, _init?: RequestInit) =>
      Response.json({}, { status: 401 }),
    );
    vi.stubGlobal("fetch", fetchMock);
    const probes = sessionProbes();
    await expect(probes.fetchMe()).resolves.toBe(401);
    expect(fetchMock.mock.calls[0]?.[0]).toBe("/api/me");
    expect(fetchMock.mock.calls[0]?.[1]?.cache).toBe("no-store");
  });

  it("count a non-2xx version as the server not serving", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response("bad gateway", { status: 502 })),
    );
    await expect(sessionProbes().fetchVersion()).rejects.toThrow();
  });

  it("read the wizard's outcome from whether the instance still needs setup", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => Response.json({ needsSetup: false })),
    );
    await expect(setupProbes().fetchMe()).resolves.toBe(401);
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => Response.json({ needsSetup: true })),
    );
    await expect(setupProbes().fetchMe()).resolves.toBe(200);
  });
});
