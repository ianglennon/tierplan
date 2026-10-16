// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient } from "@tanstack/react-query";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

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
// A read of /api/me waits for this while it is set, so a test can hold one.
let meGate: Promise<void> | undefined;
// The timers the handler asked for; a test fires them itself.
interface Timer {
  fn: () => void;
  delay: number;
  cancelled: boolean;
  fired: boolean;
}
let timers: Timer[];

const liveTimers = () => timers.filter((t) => !t.cancelled && !t.fired);
const fire = () => {
  const t = liveTimers()[0];
  if (!t) throw new Error("no timer is scheduled");
  t.fired = true;
  t.fn();
};
const schedule = (fn: () => void, delay: number) => {
  const t: Timer = { fn, delay, cancelled: false, fired: false };
  timers.push(t);
  return () => {
    t.cancelled = true;
  };
};
function holdMe(): () => void {
  let release: () => void = () => {};
  meGate = new Promise<void>((resolve) => {
    release = resolve;
  });
  return () => {
    meGate = undefined;
    release();
  };
}

function stubFetch() {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      calls.push(path);
      if (path === "/api/me") {
        await meGate;
        return Response.json(meAnswer);
      }
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
  const { handle, dispose } = createAccessChangeHandler({
    queryClient: qc,
    currentEnvId: () => current,
    moveTo,
    notifyChanged,
    notifyRefused,
    now: () => clock,
    schedule,
  });
  handlers.push({ dispose });
  return { handle, dispose, moveTo, notifyChanged, notifyRefused };
}

const meReads = () => calls.filter((c) => c === "/api/me").length;

// quiet gives everything that is able to run its turns, so that a test can then assert
// that nothing more happened while a read is held. The handler's own timers are the
// test's schedule() and never fire by themselves; the clock here is only the one that
// runs the promise chains. Call useQuietClock() first.
const useQuietClock = () => vi.useFakeTimers();
const quiet = () => vi.advanceTimersByTimeAsync(20);

beforeEach(() => {
  clock = 1_000_000;
  calls = [];
  items = [];
  recent = [];
  handlers = [];
  timers = [];
  meGate = undefined;
  qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  stubFetch();
});

afterEach(() => {
  vi.useRealTimers();
  for (const h of handlers) h.dispose();
  vi.unstubAllGlobals();
  qc.clear();
});

describe("the throttle (T-03-100)", () => {
  it("answers ten refusals inside a second with one read now and one when the window ends", async () => {
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
    // One trailing run for all nine, for the moment the window ends: the first run
    // started at +100 and the second refusal came at +200.
    expect(liveTimers()).toHaveLength(1);
    expect(liveTimers()[0]?.delay).toBe(ACCESS_REFETCH_THROTTLE_MS - 100);

    clock += ACCESS_REFETCH_THROTTLE_MS;
    fire();
    await vi.waitFor(() => expect(meReads()).toBe(2));
    await vi.waitFor(() => expect(isAccessRefetching()).toBe(false));
    expect(liveTimers()).toHaveLength(0);
    expect(timers).toHaveLength(1);
  });

  it("acts on a refusal that arrives inside the window after a run that found nothing", async () => {
    meAnswer = makeMe({ envs: { a: "viewer" } });
    const { handle, moveTo, notifyChanged } = setup("a", meAnswer);
    await handle("unseen", "/api/env/a/summary");
    expect(moveTo).not.toHaveBeenCalled();
    expect(meReads()).toBe(1);

    // The grant is withdrawn, and the next refusal comes a second later.
    meAnswer = makeMe({ envs: {}, administrators: ["admin"] });
    qc.setQueryData(["me"], meAnswer);
    clock += 1_000;
    await handle("unseen", "/api/env/a/containers");
    expect(meReads()).toBe(1);
    expect(moveTo).not.toHaveBeenCalled();

    clock += ACCESS_REFETCH_THROTTLE_MS;
    fire();
    await vi.waitFor(() => expect(moveTo).toHaveBeenCalledExactlyOnceWith(undefined));
    expect(notifyChanged).toHaveBeenCalledTimes(1);
    expect(meReads()).toBe(2);
  });

  it("never has two reads of /api/me outstanding", async () => {
    useQuietClock();
    meAnswer = makeMe({ envs: { a: "viewer" } });
    const { handle } = setup("a", meAnswer);
    const release = holdMe();
    const first = handle("unseen", "/api/env/a/summary");
    await vi.waitFor(() => expect(meReads()).toBe(1));

    clock += 1_000;
    await handle("unseen", "/api/env/a/containers");
    clock += ACCESS_REFETCH_THROTTLE_MS;
    fire();
    // The window is over but the first read has not answered.
    await quiet();
    expect(meReads()).toBe(1);

    release();
    await first;
    await vi.waitFor(() => expect(meReads()).toBe(2));
    await vi.waitFor(() => expect(isAccessRefetching()).toBe(false));
  });

  it("folds a refusal that arrives after the window into the run still outstanding (WR-G-01)", async () => {
    useQuietClock();
    const before = makeMe({ envs: { a: "viewer", b: "viewer" } });
    meAnswer = makeMe({ envs: { b: "viewer" } });
    items = ["b"];
    const { handle, moveTo, notifyChanged } = setup("a", before);
    const release = holdMe();
    const first = handle("unseen", "/api/env/a/summary");
    await vi.waitFor(() => expect(meReads()).toBe(1));

    // The window is over but the first read has not answered: a slow /api/me.
    clock += ACCESS_REFETCH_THROTTLE_MS + 1_000;
    await handle("unseen", "/api/env/a/containers");
    await quiet();
    // No second read beside the first, and the first was not cancelled.
    expect(meReads()).toBe(1);
    expect(liveTimers()).toHaveLength(1);
    expect(moveTo).not.toHaveBeenCalled();

    fire();
    await quiet();
    expect(meReads()).toBe(1);

    release();
    await first;
    await vi.waitFor(() => expect(isAccessRefetching()).toBe(false));
    await vi.waitFor(() => expect(meReads()).toBe(2));
    // The move and the toast happened once, not once per run.
    expect(moveTo).toHaveBeenCalledExactlyOnceWith("b");
    expect(notifyChanged).toHaveBeenCalledTimes(1);
  });

  it("shows the skeleton for a held environment from a folded refusal until the trailing run decides (WR-G-02)", async () => {
    meAnswer = makeMe({ envs: { a: "viewer" } });
    const { handle, moveTo, notifyChanged } = setup("a", meAnswer);
    await handle("unseen", "/api/env/a/summary");
    expect(isAccessRefetching()).toBe(false);

    // The grant is withdrawn and the page's 404 comes inside the window: the page
    // must not show Page not found for the seconds until the trailing run.
    meAnswer = makeMe({ envs: {}, administrators: ["admin"] });
    clock += 2_000;
    await handle("unseen", "/api/env/a/containers");
    expect(meReads()).toBe(1);
    expect(isAccessRefetching()).toBe(true);
    // A second folded refusal does not raise a second hold.
    await handle("unseen", "/api/env/a/networks");
    expect(isAccessRefetching()).toBe(true);

    // The trailing run takes over without a frame between.
    const release = holdMe();
    clock += ACCESS_REFETCH_THROTTLE_MS;
    fire();
    expect(isAccessRefetching()).toBe(true);
    await vi.waitFor(() => expect(meReads()).toBe(2));
    expect(isAccessRefetching()).toBe(true);
    release();
    await vi.waitFor(() => expect(moveTo).toHaveBeenCalledExactlyOnceWith(undefined));
    await vi.waitFor(() => expect(isAccessRefetching()).toBe(false));
    expect(notifyChanged).toHaveBeenCalledTimes(1);
  });

  it("shows no skeleton for a folded refusal on an environment the user never held (D-08)", async () => {
    meAnswer = makeMe({ envs: { a: "viewer" } });
    const { handle, moveTo, notifyChanged } = setup("ghost", meAnswer);
    await handle("unseen", "/api/env/ghost/summary");
    clock += 2_000;
    await handle("unseen", "/api/env/ghost/containers");
    expect(liveTimers()).toHaveLength(1);
    expect(isAccessRefetching()).toBe(false);

    clock += ACCESS_REFETCH_THROTTLE_MS;
    fire();
    await vi.waitFor(() => expect(meReads()).toBe(2));
    await vi.waitFor(() => expect(isAccessRefetching()).toBe(false));
    expect(moveTo).not.toHaveBeenCalled();
    expect(notifyChanged).not.toHaveBeenCalled();
  });

  it("lets go of the waiting skeleton when the handler is disposed", async () => {
    meAnswer = makeMe({ envs: { a: "viewer" } });
    const { handle, dispose } = setup("a", meAnswer);
    await handle("unseen", "/api/env/a/summary");
    clock += 2_000;
    await handle("unseen", "/api/env/a/containers");
    expect(isAccessRefetching()).toBe(true);
    dispose();
    expect(isAccessRefetching()).toBe(false);
  });

  it("counts the trailing run as unseen when any refusal folded into it was", async () => {
    meAnswer = makeMe({ envs: { a: "operator" } });
    const { handle, moveTo, notifyChanged, notifyRefused } = setup("a", meAnswer);
    await handle("refused", "/api/env/a/reconnect");
    expect(notifyChanged).not.toHaveBeenCalled();

    // The role shrank; then a refused action and an unseen read come in the window.
    meAnswer = makeMe({ envs: { a: "viewer" } });
    clock += 1_000;
    await handle("refused", "/api/env/a/reconnect");
    await handle("unseen", "/api/env/a/summary");
    // Every refused action is told at once, folded or not.
    expect(notifyRefused).toHaveBeenCalledTimes(2);

    clock += ACCESS_REFETCH_THROTTLE_MS;
    fire();
    await vi.waitFor(() => expect(notifyChanged).toHaveBeenCalledTimes(1));
    expect(moveTo).not.toHaveBeenCalled();
  });

  it("does not show the access toast for a trailing run of refused actions alone", async () => {
    meAnswer = makeMe({ envs: { a: "operator" } });
    const { handle, notifyChanged, notifyRefused } = setup("a", meAnswer);
    await handle("refused", "/api/env/a/reconnect");
    meAnswer = makeMe({ envs: { a: "viewer" } });
    clock += 1_000;
    await handle("refused", "/api/env/a/reconnect");
    clock += ACCESS_REFETCH_THROTTLE_MS;
    fire();
    await vi.waitFor(() => expect(meReads()).toBe(2));
    await vi.waitFor(() => expect(isAccessRefetching()).toBe(false));
    expect(notifyRefused).toHaveBeenCalledTimes(2);
    expect(notifyChanged).not.toHaveBeenCalled();
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

  it("changes nothing and shows no skeleton for an environment the user never held", async () => {
    const me = makeMe({ envs: { a: "viewer" } });
    meAnswer = me;
    const { handle, moveTo, notifyChanged } = setup("ghost", me);
    const release = holdMe();
    const run = handle("unseen", "/api/env/ghost/summary");
    await vi.waitFor(() => expect(meReads()).toBe(1));
    // The plain Page not found is not held back for a read of /api/me (D-08).
    expect(isAccessRefetching()).toBe(false);
    release();
    await run;
    expect(isAccessRefetching()).toBe(false);
    expect(moveTo).not.toHaveBeenCalled();
    expect(notifyChanged).not.toHaveBeenCalled();
  });

  it("shows no skeleton for an ID that is not an environment, even to a user who holds every environment (IN-G-03)", async () => {
    const me = makeMe({ instanceAdmin: true });
    meAnswer = me;
    qc.setQueryData(["environments"], { items: [{ id: "a", name: "a" }] });
    const { handle, moveTo, notifyChanged } = setup("ghost", me);
    const release = holdMe();
    const run = handle("unseen", "/api/env/ghost/summary");
    await vi.waitFor(() => expect(meReads()).toBe(1));
    expect(isAccessRefetching()).toBe(false);
    release();
    await run;
    expect(moveTo).not.toHaveBeenCalled();
    expect(notifyChanged).not.toHaveBeenCalled();
  });

  it("still shows the skeleton for a real environment to a user who holds every environment", async () => {
    const me = makeMe({ instanceAdmin: true });
    meAnswer = me;
    qc.setQueryData(["environments"], { items: [{ id: "a", name: "a" }] });
    const { handle } = setup("a", me);
    const release = holdMe();
    const run = handle("unseen", "/api/env/a/summary");
    await vi.waitFor(() => expect(meReads()).toBe(1));
    expect(isAccessRefetching()).toBe(true);
    release();
    await run;
    expect(isAccessRefetching()).toBe(false);
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

describe("dispose", () => {
  it("cancels the trailing run, releases the skeleton and silences a run still in flight", async () => {
    useQuietClock();
    meAnswer = makeMe({ envs: { a: "viewer", b: "viewer" } });
    const { handle, dispose, moveTo, notifyChanged } = setup("a", meAnswer);
    const release = holdMe();
    const first = handle("unseen", "/api/env/a/summary");
    await vi.waitFor(() => expect(meReads()).toBe(1));
    expect(isAccessRefetching()).toBe(true);
    clock += 1_000;
    await handle("unseen", "/api/env/a/containers");
    expect(liveTimers()).toHaveLength(1);

    dispose();
    expect(liveTimers()).toHaveLength(0);
    expect(timers[0]?.cancelled).toBe(true);
    expect(isAccessRefetching()).toBe(false);

    // The read answers with the grant gone, and nothing reacts to it.
    meAnswer = makeMe({ envs: { b: "viewer" } });
    items = ["b"];
    release();
    await first;
    await quiet();
    expect(moveTo).not.toHaveBeenCalled();
    expect(notifyChanged).not.toHaveBeenCalled();
    expect(isAccessRefetching()).toBe(false);

    // The skeleton count was released once: a new handler raises it and drops it.
    clock += 10_000;
    meAnswer = makeMe({ envs: { a: "viewer" } });
    const next = setup("a", meAnswer);
    const run = next.handle("unseen", "/api/env/a/summary");
    expect(isAccessRefetching()).toBe(true);
    await run;
    expect(isAccessRefetching()).toBe(false);
  });

  it("stops observing /api/me", () => {
    const { dispose } = setup("a", makeMe({ envs: { a: "viewer" } }));
    expect(qc.getQueryCache().hasListeners()).toBe(true);
    dispose();
    expect(qc.getQueryCache().hasListeners()).toBe(false);
  });
});
