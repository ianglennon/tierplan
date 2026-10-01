// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { createMemoryHistory, createRouter } from "@tanstack/react-router";
import { afterEach, describe, expect, it, vi } from "vitest";

import { queryClient } from "@/api/query-client";
import { routeTree } from "@/routeTree.gen";
import "@/i18n";

const ME = {
  user: { id: "u1", username: "admin", role: "admin" },
  csrfToken: "c",
  permissions: { global: [], environments: {} },
};

// stubInstance answers /api/instance, and /api/me for a signed-in or signed-out
// caller.
function stubInstance(needsSetup: boolean, signedIn = true) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      if (new URL(req.url).pathname === "/api/me") {
        return signedIn ? Response.json(ME) : Response.json({ status: 401 }, { status: 401 });
      }
      return Response.json({ instanceName: "", needsSetup, rememberDays: 30 });
    }),
  );
}

async function visit(path: string) {
  return (await go(path)).pathname;
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
  vi.unstubAllGlobals();
  queryClient.clear();
});

describe("first-run redirects", () => {
  it("sends / to /setup while no admin exists", async () => {
    stubInstance(true);
    expect(await visit("/")).toBe("/setup");
  });

  it("keeps / once setup is done and the visitor is signed in", async () => {
    stubInstance(false);
    expect(await visit("/")).toBe("/");
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

  it("sends /setup back to / once an admin exists", async () => {
    stubInstance(false);
    expect(await visit("/setup")).toBe("/");
  });

  it("sends /setup on to /login once an admin exists and nobody is signed in", async () => {
    stubInstance(false, false);
    expect(await visit("/setup")).toBe("/login");
  });

  it("asks the server again on every visit instead of trusting an earlier answer", async () => {
    stubInstance(true);
    expect(await visit("/")).toBe("/setup");
    stubInstance(false);
    expect(await visit("/")).toBe("/");
  });
});
