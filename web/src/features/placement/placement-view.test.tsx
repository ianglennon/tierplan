// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import {
  PLACEMENT_REFETCH_MS,
  placementKey,
  placementQuery,
  placementRetry,
} from "@/api/placement";
import { queryClient } from "@/api/query-client";
import { servicesKey } from "@/api/services";
import { TooltipProvider } from "@/components/ui/tooltip";
import { node, placement, task } from "@/features/placement/fixtures";
import type { PlacementSearch, PlacementSearchPatch } from "@/features/placement/placement-search";
import { PlacementView } from "@/features/placement/placement-view";
import "@/i18n";
import { makeMe } from "@/test-me";
import { splitPermissions } from "@/test-permissions";
import { TestRouter } from "@/test-router";

// The Placement page: its states (E9), the filters in the URL and the polling (plan 06-11,
// RES-12, D-18, D-19, UI-SPEC "Placement views").

const ENV = "e1";
const defaults = queryClient.getDefaultOptions();

let me = makeMe({ envs: { [ENV]: "viewer" } });
let answer: () => Response | Promise<Response>;
let calls = 0;

beforeEach(() => {
  me = makeMe({ envs: { [ENV]: "viewer" } });
  calls = 0;
  answer = () => Response.json(DATA);
  queryClient.setDefaultOptions({ queries: { retry: false, refetchOnWindowFocus: false } });
  placementRetry.attempts = 0;
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      if (url.pathname === "/api/me") return Response.json(me);
      if (url.pathname === "/api/environments") {
        return Response.json({ items: [{ id: ENV, name: "Prod swarm", envType: "swarm" }] });
      }
      if (url.pathname === `/api/env/${ENV}/placement`) {
        calls++;
        return answer();
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.useRealTimers();
  queryClient.clear();
  queryClient.setDefaultOptions(defaults);
  placementRetry.attempts = 3;
});

const DATA = placement(
  [node({ id: "n1", hostname: "mgr-1" }), node({ id: "n2", hostname: "wrk-1", role: "worker" })],
  [
    task({ id: "a", nodeId: "n1", service: "web", serviceId: "svc-web", stack: "shop" }),
    task({ id: "b", nodeId: "n2", service: "db", serviceId: "svc-db", stack: "shop", slot: 1 }),
    task({ id: "c", nodeId: "n2", service: "solo", serviceId: "svc-solo", stack: "" }),
  ],
  {
    services: [
      { id: "svc-web", name: "web", stack: "shop", mode: "replicated", constraints: [] },
      { id: "svc-db", name: "db", stack: "shop", mode: "replicated", constraints: [] },
      { id: "svc-solo", name: "solo", stack: "", mode: "replicated", constraints: [] },
    ],
  },
);

function renderView(
  search: PlacementSearch = {},
  onSearchChange: (p: PlacementSearchPatch) => void = () => {},
) {
  return render(
    <QueryClientProvider client={queryClient}>
      <TooltipProvider>
        <TestRouter>
          <PlacementView envId={ENV} search={search} onSearchChange={onSearchChange} />
        </TestRouter>
      </TooltipProvider>
    </QueryClientProvider>,
  );
}

describe("PlacementView", () => {
  it("names the page and the environment, and draws a column per node", async () => {
    renderView();
    expect(await screen.findByRole("region", { name: "mgr-1" })).toBeTruthy();
    expect(screen.getByRole("heading", { level: 1, name: "Placement" })).toBeTruthy();
    expect(screen.getByRole("region", { name: "wrk-1" })).toBeTruthy();
    expect(screen.getByText("Where tasks run across Prod swarm.")).toBeTruthy();
    expect(within(screen.getByRole("region", { name: "wrk-1" })).getAllByRole("link")).toHaveLength(
      2,
    );
  });

  it("draws the toolbar and three skeleton columns at once, before the answer", async () => {
    answer = () => new Promise<Response>(() => {});
    const { container } = renderView();
    expect(await screen.findByRole("searchbox", { name: "Search services" })).toBeTruthy();
    expect(screen.getByRole("radio", { name: "By node" })).toBeTruthy();
    expect(container.querySelectorAll("[data-slot=node-columns-skeleton] > div")).toHaveLength(3);
  });

  it("says why there is nothing when the swarm has no tasks", async () => {
    answer = () => Response.json(placement([node({ id: "n1", hostname: "mgr-1" })], []));
    renderView();
    expect(await screen.findByText("No tasks to place")).toBeTruthy();
    expect(screen.queryByRole("region")).toBeNull();
  });

  it("offers Clear filters when the filters leave nothing, and clears both", async () => {
    const change = vi.fn();
    renderView({ stack: "blog", q: "zzz" }, change);
    expect(await screen.findByText("No tasks match these filters.")).toBeTruthy();
    fireEvent.click(screen.getAllByRole("button", { name: /Clear filters/ })[0] as HTMLElement);
    expect(change).toHaveBeenCalledWith({ stack: undefined, q: undefined });
  });

  it("shows a load error with Retry loading placement, which asks again", async () => {
    answer = () => Response.json({ title: "boom", status: 500 }, { status: 500 });
    renderView();
    expect(await screen.findByText("Couldn't load placement.")).toBeTruthy();
    const before = calls;
    answer = () => Response.json(DATA);
    fireEvent.click(screen.getByRole("button", { name: "Retry loading placement" }));
    expect(await screen.findByRole("region", { name: "mgr-1" })).toBeTruthy();
    expect(calls).toBeGreaterThan(before);
  });

  it("tells a standalone host it has no swarm", async () => {
    answer = () =>
      Response.json(
        { type: "not_a_swarm", title: "Not a swarm", status: 409 },
        { status: 409, headers: { "Content-Type": "application/problem+json" } },
      );
    renderView();
    expect(await screen.findByRole("heading", { name: /swarm/i })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Retry loading placement" })).toBeNull();
  });

  it("shows no page to a user who may not read services", async () => {
    me = makeMe({
      envs: { [ENV]: "viewer" },
      permissions: splitPermissions(["containers.read", "environments.read"]),
    });
    renderView();
    await waitFor(() => expect(screen.queryByRole("heading", { name: "Placement" })).toBeNull());
    expect(calls).toBe(0);
  });

  it("filters by service name and lists the stacks in the Stack select", async () => {
    const change = vi.fn();
    renderView({ stack: "shop", q: "web" }, change);
    const region = await screen.findByRole("region", { name: "mgr-1" });
    expect(within(region).getAllByRole("link")).toHaveLength(1);
    // The other node stays, empty.
    expect(
      within(screen.getByRole("region", { name: "wrk-1" })).getByText("No tasks"),
    ).toBeTruthy();
    fireEvent.click(screen.getByRole("combobox", { name: "Stack" }));
    expect(await screen.findByRole("option", { name: "All stacks" })).toBeTruthy();
    expect(screen.getByRole("option", { name: "shop" })).toBeTruthy();
    expect(screen.getByRole("option", { name: "Not in a stack" })).toBeTruthy();
    fireEvent.click(screen.getByRole("option", { name: "Not in a stack" }));
    expect(change).toHaveBeenCalledWith({ stack: "none" });
  });

  it("shows Clear filters only while a filter is on", async () => {
    const { unmount } = renderView();
    await screen.findByRole("region", { name: "mgr-1" });
    expect(screen.queryByRole("button", { name: "Clear filters" })).toBeNull();
    unmount();
    renderView({ q: "web" });
    await screen.findByRole("region", { name: "mgr-1" });
    expect(screen.getByRole("button", { name: "Clear filters" })).toBeTruthy();
  });
});

describe("placementQuery", () => {
  it("is under the services list key, so a service event refreshes it", () => {
    const key = placementKey(ENV);
    expect(key.slice(0, servicesKey(ENV).length)).toEqual([...servicesKey(ENV)]);
    expect(placementKey(ENV, "svc-web")).not.toEqual(key);
  });

  it("polls every 5 s while the page is visible and not while it is hidden", () => {
    const opts = placementQuery(ENV);
    const every = opts.refetchInterval as () => number | false;
    const original = Object.getOwnPropertyDescriptor(Document.prototype, "visibilityState");
    try {
      Object.defineProperty(Document.prototype, "visibilityState", {
        configurable: true,
        get: () => "visible",
      });
      expect(every()).toBe(PLACEMENT_REFETCH_MS);
      expect(PLACEMENT_REFETCH_MS).toBe(5_000);
      Object.defineProperty(Document.prototype, "visibilityState", {
        configurable: true,
        get: () => "hidden",
      });
      expect(every()).toBe(false);
    } finally {
      if (original) Object.defineProperty(Document.prototype, "visibilityState", original);
    }
  });

  it("asks for one service when given one", async () => {
    const seen: string[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (req: Request) => {
        seen.push(new URL(req.url).search);
        return Response.json(DATA);
      }),
    );
    await act(async () => {
      await queryClient.fetchQuery(placementQuery(ENV, "svc-web"));
    });
    expect(seen).toEqual(["?serviceId=svc-web"]);
  });
});
