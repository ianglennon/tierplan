// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { createMemoryHistory, createRouter } from "@tanstack/react-router";
import { afterEach, describe, expect, it, vi } from "vitest";

import { queryClient } from "@/api/query-client";
import { routeTree } from "@/routeTree.gen";
import "@/i18n";

function stubInstance(needsSetup: boolean) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => Response.json({ instanceName: "", needsSetup, rememberDays: 30 })),
  );
}

async function visit(path: string) {
  const router = createRouter({
    routeTree,
    history: createMemoryHistory({ initialEntries: [path] }),
  });
  await router.load();
  return router.state.location.pathname;
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

  it("keeps / once setup is done", async () => {
    stubInstance(false);
    expect(await visit("/")).toBe("/");
  });

  it("opens /setup while setup is needed", async () => {
    stubInstance(true);
    expect(await visit("/setup")).toBe("/setup");
  });

  it("sends /setup back to / once an admin exists", async () => {
    stubInstance(false);
    expect(await visit("/setup")).toBe("/");
  });

  it("asks the server again on every visit instead of trusting an earlier answer", async () => {
    stubInstance(true);
    expect(await visit("/")).toBe("/setup");
    stubInstance(false);
    expect(await visit("/")).toBe("/");
  });
});
