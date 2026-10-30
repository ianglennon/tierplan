// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { queryClient } from "@/api/query-client";
import type { Me } from "@/hooks/use-me";
import { recordRecentTiming } from "@/routes/_app/env/$envId";
import { routeTree } from "@/routeTree.gen";
import "@/i18n";
import { makeMe } from "@/test-me";
import { waitUnderFakeClock } from "@/test-settle";

const ME = makeMe({
  instanceAdmin: true,
  csrfToken: "c",
  permissions: { global: [], environments: { "*": ["environments.read"] } },
});

function env(id: string, name: string) {
  return {
    id,
    name,
    kind: "agent",
    enabled: true,
    isSelfHost: false,
    isLocalSocket: false,
    createdAt: "2026-10-01T10:00:00Z",
    engine: {},
    status: { state: "online" },
    agentVersion: null,
    agents: null,
    engineVersion: null,
    failingServices: null,
    lastSeen: null,
    nodes: null,
    updateAvailable: false,
  };
}

const SUMMARY = (id: string, name: string) => ({
  environment: { id, name, status: { state: "online" } },
  engine: { name: "Docker Engine", version: "29.8.2", apiVersion: "1.54" },
  counts: {
    containers: 0,
    containersRunning: 0,
    containersStopped: 0,
    images: 0,
    volumes: 0,
    networks: 0,
  },
  isWorker: false,
  fetchedAt: "2026-10-01T10:00:00Z",
});

let sent: { method: string; path: string }[];
// What GET /api/me answers; stubInstance puts it back to the instance admin.
let meBody: Me = ME;
// What GET /api/me/recent-environments and GET /api/environments answer.
let recent: () => Response | Promise<Response>;
let list: () => Response | Promise<Response>;

// stubInstance answers /api/instance, /api/me for a signed-in or signed-out
// caller, the two lists "/" reads, and the dashboard reads of an environment.
function stubInstance(needsSetup: boolean, signedIn = true) {
  sent = [];
  meBody = ME;
  recent = () => Response.json({ environmentIds: [] });
  list = () => Response.json({ items: [] });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      sent.push({ method: req.method, path });
      if (path === "/api/me") {
        return signedIn ? Response.json(meBody) : Response.json({ status: 401 }, { status: 401 });
      }
      if (path === "/api/me/recent-environments") return recent();
      if (path === "/api/environments") return list();
      const summary = /^\/api\/env\/([^/]+)\/summary$/.exec(path);
      if (summary) return Response.json(SUMMARY(summary[1] ?? "", `env ${summary[1]}`));
      if (path.startsWith("/api/me/recent-environments/"))
        return new Response(null, { status: 204 });
      if (path.startsWith("/api/env/")) return Response.json({ items: [] });
      return Response.json({ instanceName: "", needsSetup, rememberDays: 30 });
    }),
  );
}

async function visit(path: string) {
  return (await go(path)).pathname;
}

// land renders the app at a path and returns where it ends up. The home page
// redirects from a component, so it needs rendering, not only loading.
async function land(path: string) {
  const router = createRouter({
    routeTree,
    history: createMemoryHistory({ initialEntries: [path] }),
  });
  render(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
  // Past the path itself and past "/", which /setup passes through on its way.
  await waitFor(() => expect(["/", path]).not.toContain(router.state.location.pathname));
  return router.state.location.pathname;
}

async function go(path: string) {
  const router = createRouter({
    routeTree,
    history: createMemoryHistory({ initialEntries: [path] }),
  });
  await router.load();
  return router.state.location;
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  queryClient.clear();
});

describe("first-run redirects", () => {
  it("sends / to /setup while no admin exists", async () => {
    stubInstance(true);
    expect(await visit("/")).toBe("/setup");
  });

  it("sends / to the overview once setup is done and the visitor is signed in", async () => {
    stubInstance(false);
    expect(await land("/")).toBe("/environments");
  });

  it("sends / to /login when setup is done and nobody is signed in", async () => {
    stubInstance(false, false);
    expect(await visit("/")).toBe("/login");
  });

  it("sends the home page to /login without a next to come back to", async () => {
    stubInstance(false, false);
    const location = await go("/");
    expect(location.pathname).toBe("/login");
    // The home page is the default, so there is nothing to remember.
    expect(location.search).not.toHaveProperty("next");
  });

  it("opens /setup while setup is needed", async () => {
    stubInstance(true);
    expect(await visit("/setup")).toBe("/setup");
  });

  it("sends /setup back to the home page once an admin exists", async () => {
    stubInstance(false);
    expect(await land("/setup")).toBe("/environments");
  });

  it("sends /setup on to /login once an admin exists and nobody is signed in", async () => {
    stubInstance(false, false);
    expect(await visit("/setup")).toBe("/login");
  });

  it("asks the server again on every visit instead of trusting an earlier answer", async () => {
    stubInstance(true);
    expect(await visit("/")).toBe("/setup");
    cleanup();
    stubInstance(false);
    expect(await land("/")).toBe("/environments");
  });
});

describe("a user with no access to any environment (D-08)", () => {
  function renderHome() {
    const router = createRouter({
      routeTree,
      history: createMemoryHistory({ initialEntries: ["/"] }),
    });
    render(
      <QueryClientProvider client={queryClient}>
        <RouterProvider router={router} />
      </QueryClientProvider>,
    );
    return router;
  }

  it("stays on the home page and says who to ask, inside the shell", async () => {
    stubInstance(false);
    meBody = makeMe({ envs: {}, administrators: ["admin", "ops-lead"] });
    const router = renderHome();
    expect(
      await screen.findByRole("heading", { name: "You don't have access to any environments yet" }),
    ).toBeTruthy();
    expect(screen.getByText("Ask an administrator to give you access.")).toBeTruthy();
    expect(screen.getByText("Administrators: admin, ops-lead")).toBeTruthy();
    // The shell works: the user menu is there and the page did not redirect.
    expect(screen.getByRole("button", { name: "User menu" })).toBeTruthy();
    expect(router.state.location.pathname).toBe("/");
  });

  it("still redirects a user who can read an environment", async () => {
    stubInstance(false);
    meBody = makeMe({ envs: { a: "viewer" } });
    recent = () => Response.json({ environmentIds: ["a"] });
    list = () => Response.json({ items: [env("a", "alpha")] });
    expect(await land("/")).toBe("/env/a");
  });
});

describe("the last-used environment (D-31)", () => {
  it("sends / to the most recent environment when the user can still read it", async () => {
    stubInstance(false);
    recent = () => Response.json({ environmentIds: ["b", "a"] });
    list = () => Response.json({ items: [env("a", "alpha"), env("b", "bravo")] });
    expect(await land("/")).toBe("/env/b");
  });

  it("skips a recent environment that is no longer in the list and takes the next", async () => {
    stubInstance(false);
    recent = () => Response.json({ environmentIds: ["gone", "a"] });
    list = () => Response.json({ items: [env("a", "alpha")] });
    expect(await land("/")).toBe("/env/a");
  });

  it("sends / to the overview when the user has no recent environment", async () => {
    stubInstance(false);
    list = () => Response.json({ items: [env("a", "alpha")] });
    expect(await land("/")).toBe("/environments");
  });

  it("sends / to the overview when the only recent environment was removed", async () => {
    stubInstance(false);
    recent = () => Response.json({ environmentIds: ["x"] });
    list = () => Response.json({ items: [env("a", "alpha")] });
    expect(await land("/")).toBe("/environments");
  });

  it("sends / to the overview when the recent list cannot be read", async () => {
    stubInstance(false);
    recent = () => new Response("boom", { status: 500 });
    list = () => Response.json({ items: [env("a", "alpha")] });
    expect(await land("/")).toBe("/environments");
  });

  it("sends / to the overview, which says so, when the environment list cannot be read", async () => {
    stubInstance(false);
    recent = () => Response.json({ environmentIds: ["a"] });
    list = () => new Response("boom", { status: 500 });
    expect(await land("/")).toBe("/environments");
  });

  it("shows a skeleton, never the empty state, while the recent list loads", async () => {
    stubInstance(false);
    recent = () => new Promise<Response>(() => {});
    const router = createRouter({
      routeTree,
      history: createMemoryHistory({ initialEntries: ["/"] }),
    });
    render(
      <QueryClientProvider client={queryClient}>
        <RouterProvider router={router} />
      </QueryClientProvider>,
    );
    expect(await screen.findByLabelText("Loading environments")).toBeTruthy();
    expect(screen.queryByText("No environments yet")).toBeNull();
    expect(router.state.location.pathname).toBe("/");
  });
});

describe("recording the environment that was opened (D-31)", () => {
  beforeEach(() => {
    recordRecentTiming.delayMs = 30;
  });
  afterEach(() => {
    recordRecentTiming.delayMs = 2_000;
    vi.useRealTimers();
  });

  // The record waits on a timer, so the tests that look for a record that must not come run the
  // clock themselves. The clock is fully fake: real time passing between two statements must not
  // be able to fire the record, or "nothing is recorded yet" would hold only on a quiet machine.
  // Under it the tests wait with vi.waitFor, which polls on real timers and moves the fake clock
  // 50 ms a poll, because Testing Library's waitFor and findBy poll on the faked timers and so
  // never poll, and a query's notifications are timers too.
  const fakeClock = () => vi.useFakeTimers();
  const heading = (name: string) => waitUnderFakeClock(() => screen.getByRole("heading", { name }));
  // settled(queryClient) for a fake clock: no query is fetching and no mutation is running.
  const noneRunning = () =>
    waitUnderFakeClock(() => expect(queryClient.isFetching() + queryClient.isMutating()).toBe(0));

  const puts = () =>
    sent
      .filter((s) => s.method === "PUT")
      .map((s) => s.path.replace("/api/me/recent-environments/", ""));

  it("records an environment once, however often the layout renders", async () => {
    fakeClock();
    stubInstance(false);
    list = () => Response.json({ items: [env("a", "alpha")] });
    render(
      <QueryClientProvider client={queryClient}>
        <RouterProvider
          router={createRouter({
            routeTree,
            history: createMemoryHistory({ initialEntries: ["/env/a"] }),
          })}
        />
      </QueryClientProvider>,
    );
    await heading("env a");
    // The layout renders again as its queries refresh; none of it records again.
    await act(async () => {
      await queryClient.invalidateQueries({ queryKey: ["environments"] });
    });
    await waitUnderFakeClock(() => expect(puts()).toEqual(["a"]));
    await noneRunning();
    // Far past the delay: a second timer would have fired by now.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1_000);
    });
    expect(puts()).toEqual(["a"]);
  });

  it("records only the environment the user stayed on", async () => {
    fakeClock();
    stubInstance(false);
    list = () => Response.json({ items: [env("a", "alpha"), env("b", "bravo")] });
    // Ten minutes of fake time. The clock moves only when the test moves it, and vi.waitFor moves
    // it 50 ms a poll, so ten minutes is out of reach of any wait that ends before the test
    // timeout: only the explicit advance below fires the timer.
    recordRecentTiming.delayMs = 600_000;
    const router = createRouter({
      routeTree,
      history: createMemoryHistory({ initialEntries: ["/env/a"] }),
    });
    render(
      <QueryClientProvider client={queryClient}>
        <RouterProvider router={router} />
      </QueryClientProvider>,
    );
    await heading("env a");
    await act(async () => {
      await router.navigate({ to: "/env/$envId", params: { envId: "b" } });
    });
    await heading("env b");
    // Nothing is recorded while the delay has not passed, a's timer was cancelled by leaving it.
    expect(puts()).toEqual([]);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(600_000);
    });
    await waitUnderFakeClock(() => expect(puts()).toEqual(["b"]));
    await noneRunning();
    expect(puts()).toEqual(["b"]);
  });

  it("does not mind that recording fails", async () => {
    fakeClock();
    stubInstance(false);
    list = () => Response.json({ items: [env("a", "alpha")] });
    const original = globalThis.fetch;
    let refused = 0;
    vi.stubGlobal("fetch", async (req: Request) => {
      if (req.method !== "PUT") return original(req);
      refused++;
      return new Response("boom", { status: 500 });
    });
    render(
      <QueryClientProvider client={queryClient}>
        <RouterProvider
          router={createRouter({
            routeTree,
            history: createMemoryHistory({ initialEntries: ["/env/a"] }),
          })}
        />
      </QueryClientProvider>,
    );
    await heading("env a");
    // The record is attempted and refused, and only then is the page looked at.
    await waitUnderFakeClock(() => expect(refused).toBe(1));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1_000);
    });
    await noneRunning();
    expect(screen.getByRole("heading", { name: "env a" })).toBeTruthy();
  });
});
