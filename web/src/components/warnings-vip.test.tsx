// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  Outlet,
  RouterProvider,
} from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { queryClient } from "@/api/query-client";
import { HealthBell } from "@/components/warnings";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { EnvHealthStreamProvider } from "@/features/environments/health-stream";
import { meQueryOptions } from "@/hooks/use-me";
import "@/i18n";
import { makeMe } from "@/test-me";

// An Admin of one environment reads its bell. The server sends the virtual IP items only to such a caller
// (26-26); the web draws whatever it is sent.
const READER = makeMe({
  permissions: { global: [], environments: { "*": ["environments.read", "ingress.manage"] } },
});

let envItems: Array<Record<string, unknown>> = [];

beforeEach(() => {
  envItems = [];
  setCsrfToken("csrf-1");
  FakeEventSource.reset();
  vi.stubGlobal("EventSource", FakeEventSource);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(READER);
      if (path === "/api/warnings") return Response.json({ items: [] });
      if (path === "/api/environments/warnings") return Response.json({ items: envItems });
      if (path === "/api/registries/alerts") return Response.json({ items: [] });
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  queryClient.clear();
  setCsrfToken(null);
});

function renderBell() {
  queryClient.setQueryData(meQueryOptions.queryKey, READER);
  const root = createRootRoute({
    component: () => (
      <>
        <HealthBell />
        <Outlet />
      </>
    ),
  });
  const home = createRoute({ getParentRoute: () => root, path: "/", component: () => null });
  const router = createRouter({
    routeTree: root.addChildren([home]),
    history: createMemoryHistory({ initialEntries: ["/"] }),
  });
  render(
    <QueryClientProvider client={queryClient}>
      <EnvHealthStreamProvider>
        <RouterProvider router={router} />
      </EnvHealthStreamProvider>
    </QueryClientProvider>,
  );
}

// ago is an instant this long before now, which the bell words as "3 h ago" and the like.
const ago = (ms: number) => new Date(Date.now() - ms).toISOString();
const HOUR = 3_600_000;

// The bell opened on the items the server sent, and the one item of the given warning id.
async function itemOf(id: string): Promise<{ title: string; body: string; el: HTMLElement }> {
  renderBell();
  fireEvent.click(await screen.findByRole("button", { name: /^Warnings, \d+ items?$/ }));
  const el = await screen.findByRole("listitem");
  expect(el.getAttribute("data-warning")).toBe(id);
  const title = el.querySelector("[title]")?.textContent ?? "";
  return { title, body: el.textContent?.slice(title.length) ?? "", el };
}

describe("virtual IP bell items (tracer)", () => {
  it("draws an address nobody holds: its title, its words and nothing to click", async () => {
    envItems = [
      {
        id: "vip_no_holder",
        severity: "error",
        envId: "e1",
        envName: "Prod swarm",
        since: ago(3 * HOUR),
        vip: { address: "192.168.1.250/24" },
      },
    ];
    const { title, body, el } = await itemOf("vip_no_holder");
    expect(title).toBe("No node holds the virtual IP on Prod swarm");
    expect(body).toBe(
      "No node holds 192.168.1.250/24, so ingress doesn't answer on it. This began 3 h ago. Check that a proxy is ready on at least one node.",
    );
    expect(el.querySelector("a, button")).toBeNull();
  });
});

describe("virtual IP bell items (empty)", () => {
  it("draws nothing for an environment with no virtual IP condition", async () => {
    renderBell();
    fireEvent.click(await screen.findByRole("button", { name: "Warnings" }));
    expect(await screen.findByText("No warnings")).toBeTruthy();
  });
});
