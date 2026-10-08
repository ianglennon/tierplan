// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient } from "@tanstack/react-query";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { AccessChangeKind } from "@/api/client";
import {
  ACCESS_REFETCH_THROTTLE_MS,
  createAccessChangeHandler,
  isAccessRefetching,
} from "@/hooks/use-access-refetch";
import type { Me } from "@/hooks/use-me";
import { makeMe } from "@/test-me";

let clock: number;
let meAnswer: Me;
let items: string[];
let recent: string[];
let calls: string[];
let qc: QueryClient;
let handlers: { dispose: () => void }[];

function stubFetch() {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      calls.push(path);
      if (path === "/api/me") return Response.json(meAnswer);
      if (path === "/api/environments") {
        return Response.json({ items: items.map((id) => ({ id, name: id })) });
      }
      if (path === "/api/me/recent-environments") return Response.json({ environmentIds: recent });
      return Response.json({}, { status: 404 });
    }),
  );
}

// The environment page the user is on; a test moves the user by assigning it.
let current: string | undefined;

function setup(envId: string | undefined, before: Me) {
  current = envId;
  qc.setQueryData(["me"], before);
  const moveTo = vi.fn();
  const notifyChanged = vi.fn();
  const notifyRefused = vi.fn();
  const created = createAccessChangeHandler({
    queryClient: qc,
    currentEnvId: () => current,
    moveTo,
    notifyChanged,
    notifyRefused,
    now: () => clock,
  }) as unknown as
    | ((kind: AccessChangeKind, path: string) => Promise<void>)
    | {
        handle: (kind: AccessChangeKind, path: string) => Promise<void>;
        dispose: () => void;
      };
  const handle = typeof created === "function" ? created : created.handle;
  if (typeof created !== "function") handlers.push(created);
  return { handle, moveTo, notifyChanged, notifyRefused };
}

const meReads = () => calls.filter((c) => c === "/api/me").length;

beforeEach(() => {
  clock = 1_000_000;
  calls = [];
  items = [];
  recent = [];
  handlers = [];
  qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  stubFetch();
});

afterEach(() => {
  for (const h of handlers) h.dispose();
  vi.unstubAllGlobals();
  qc.clear();
});

describe("the throttle (T-03-100)", () => {
  it("refetches /api/me once for ten refusals inside a second, and again after the window", async () => {
    const me = makeMe({ envs: { a: "viewer" } });
    meAnswer = me;
    const { handle } = setup("a", me);
    const runs: Promise<void>[] = [];
    for (let i = 0; i < 10; i++) {
      clock += 100;
      runs.push(handle("unseen", "/api/env/a/summary"));
    }
    await Promise.all(runs);
    expect(meReads()).toBe(1);

    clock += ACCESS_REFETCH_THROTTLE_MS + 1_000;
    await handle("unseen", "/api/env/a/summary");
    expect(meReads()).toBe(2);
  });

  it("uses a window of five seconds", () => {
    expect(ACCESS_REFETCH_THROTTLE_MS).toBe(5000);
  });
});

describe("the move after an environment is lost (D-10)", () => {
  it("opens the last-used environment that is still allowed", async () => {
    const before = makeMe({ envs: { a: "viewer", b: "viewer", c: "viewer" } });
    meAnswer = makeMe({ envs: { b: "viewer", c: "viewer" } });
    items = ["b", "c"];
    recent = ["a", "c", "b"];
    const { handle, moveTo, notifyChanged } = setup("a", before);
    await handle("unseen", "/api/env/a/summary");
    expect(moveTo).toHaveBeenCalledExactlyOnceWith("c");
    expect(notifyChanged).toHaveBeenCalledTimes(1);
  });

  it("opens the first allowed environment when no recent one is allowed", async () => {
    const before = makeMe({ envs: { a: "viewer", b: "viewer" } });
    meAnswer = makeMe({ envs: { b: "viewer" } });
    items = ["b"];
    recent = ["a"];
    const { handle, moveTo } = setup("a", before);
    await handle("unseen", "/api/env/a/summary");
    expect(moveTo).toHaveBeenCalledExactlyOnceWith("b");
  });

  it("goes home, where the no-access state shows, when nothing is left", async () => {
    const before = makeMe({ envs: { a: "viewer" } });
    meAnswer = makeMe({ envs: {}, administrators: ["admin"] });
    const { handle, moveTo, notifyChanged } = setup("a", before);
    await handle("unseen", "/api/env/a/summary");
    expect(moveTo).toHaveBeenCalledExactlyOnceWith(undefined);
    expect(notifyChanged).toHaveBeenCalledTimes(1);
  });

  it("says nothing and stays put when the environment is still allowed", async () => {
    const before = makeMe({ envs: { a: "operator" } });
    meAnswer = makeMe({ envs: { a: "viewer" } });
    const { handle, moveTo, notifyChanged } = setup("a", before);
    await handle("unseen", "/api/env/a/summary");
    expect(moveTo).not.toHaveBeenCalled();
    // The role changed, so the person is told, but not moved.
    expect(notifyChanged).toHaveBeenCalledTimes(1);
  });

  it("changes nothing for an environment that never existed", async () => {
    const me = makeMe({ envs: { a: "viewer" } });
    meAnswer = me;
    const { handle, moveTo, notifyChanged } = setup("ghost", me);
    await handle("unseen", "/api/env/ghost/summary");
    expect(moveTo).not.toHaveBeenCalled();
    expect(notifyChanged).not.toHaveBeenCalled();
  });

  it("does not move a user who is not on an environment page", async () => {
    const before = makeMe({ envs: { a: "viewer", b: "viewer" } });
    meAnswer = makeMe({ envs: { b: "viewer" } });
    const { handle, moveTo } = setup(undefined, before);
    await handle("unseen", "/api/environments/a");
    expect(moveTo).not.toHaveBeenCalled();
  });

  it("reports the refetch as pending until the move is done", async () => {
    const before = makeMe({ envs: { a: "viewer", b: "viewer" } });
    meAnswer = makeMe({ envs: { b: "viewer" } });
    items = ["b"];
    const { handle } = setup("a", before);
    expect(isAccessRefetching()).toBe(false);
    const run = handle("unseen", "/api/env/a/summary");
    expect(isAccessRefetching()).toBe(true);
    await run;
    expect(isAccessRefetching()).toBe(false);
  });
});

describe("a refused action (D-10)", () => {
  it("shows the refusal every time, refetches /api/me once, and moves nobody who still has the environment", async () => {
    const me = makeMe({ envs: { a: "viewer" } });
    meAnswer = me;
    const { handle, moveTo, notifyRefused, notifyChanged } = setup("a", me);
    await Promise.all([
      handle("refused", "/api/env/a/reconnect"),
      handle("refused", "/api/env/a/reconnect"),
    ]);
    expect(notifyRefused).toHaveBeenCalledTimes(2);
    expect(meReads()).toBe(1);
    expect(moveTo).not.toHaveBeenCalled();
    expect(notifyChanged).not.toHaveBeenCalled();
  });

  it("leaves a mid-flight failure of /api/me alone", async () => {
    const me = makeMe({ envs: { a: "viewer" } });
    const { handle, moveTo } = setup("a", me);
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response("boom", { status: 500 })),
    );
    await handle("unseen", "/api/env/a/summary");
    expect(moveTo).not.toHaveBeenCalled();
    expect(isAccessRefetching()).toBe(false);
  });
});

describe("the baseline (G-03-2)", () => {
  it("moves and tells a user whose /api/me was already re-read when the refusal arrives", async () => {
    // Opening a page mounts components that read /api/me, and the server audits a
    // refusal before it sends it, so the cache already holds the new access.
    meAnswer = makeMe({ envs: { b: "viewer" } });
    items = ["b"];
    const { handle, moveTo, notifyChanged } = setup(
      "a",
      makeMe({ envs: { a: "viewer", b: "viewer" } }),
    );
    qc.setQueryData(["me"], meAnswer);
    await handle("unseen", "/api/env/a/containers");
    expect(moveTo).toHaveBeenCalledExactlyOnceWith("b");
    expect(notifyChanged).toHaveBeenCalledTimes(1);
  });

  it("tells a user whose role shrank when /api/me was already re-read, without moving them", async () => {
    meAnswer = makeMe({ envs: { a: "viewer" } });
    const { handle, moveTo, notifyChanged } = setup("a", makeMe({ envs: { a: "operator" } }));
    qc.setQueryData(["me"], meAnswer);
    await handle("unseen", "/api/env/a/containers");
    expect(notifyChanged).toHaveBeenCalledTimes(1);
    expect(moveTo).not.toHaveBeenCalled();
  });

  it("remembers an environment gained and then lost through re-reads of /api/me", async () => {
    const b = makeMe({ envs: { b: "viewer" } });
    meAnswer = b;
    items = ["b"];
    const { handle, moveTo, notifyChanged } = setup("a", b);
    qc.setQueryData(["me"], makeMe({ envs: { a: "viewer", b: "viewer" } }));
    qc.setQueryData(["me"], b);
    await handle("unseen", "/api/env/a/summary");
    expect(moveTo).toHaveBeenCalledExactlyOnceWith("b");
    expect(notifyChanged).toHaveBeenCalledTimes(1);
  });

  it("says nothing the second time after the user was told about a loss", async () => {
    meAnswer = makeMe({ envs: { b: "viewer" } });
    items = ["b"];
    const { handle, moveTo, notifyChanged } = setup(
      "a",
      makeMe({ envs: { a: "viewer", b: "viewer" } }),
    );
    await handle("unseen", "/api/env/a/summary");
    expect(moveTo).toHaveBeenCalledTimes(1);
    expect(notifyChanged).toHaveBeenCalledTimes(1);

    clock += ACCESS_REFETCH_THROTTLE_MS + 1_000;
    await handle("unseen", "/api/env/a/summary");
    expect(moveTo).toHaveBeenCalledTimes(1);
    expect(notifyChanged).toHaveBeenCalledTimes(1);
  });

  it("does not forget a lost environment because of a refusal elsewhere that said nothing", async () => {
    meAnswer = makeMe({ envs: { b: "viewer" } });
    items = ["b"];
    const { handle, moveTo, notifyChanged } = setup(
      "a",
      makeMe({ envs: { a: "viewer", b: "viewer" } }),
    );
    qc.setQueryData(["me"], meAnswer);
    current = "ghost";
    await handle("unseen", "/api/env/ghost/summary");
    expect(moveTo).not.toHaveBeenCalled();
    expect(notifyChanged).not.toHaveBeenCalled();

    clock += ACCESS_REFETCH_THROTTLE_MS + 1_000;
    current = "a";
    await handle("unseen", "/api/env/a/summary");
    expect(moveTo).toHaveBeenCalledExactlyOnceWith("b");
    expect(notifyChanged).toHaveBeenCalledTimes(1);
  });

  it("starts again for a different user", async () => {
    meAnswer = makeMe({ id: "u9", envs: { b: "viewer" } });
    const { handle, moveTo, notifyChanged } = setup("a", makeMe({ envs: { a: "viewer" } }));
    qc.setQueryData(["me"], meAnswer);
    await handle("unseen", "/api/env/a/summary");
    expect(moveTo).not.toHaveBeenCalled();
    expect(notifyChanged).not.toHaveBeenCalled();
  });
});
