// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { EnvironmentItem } from "@/api/environments";
import { queryClient } from "@/api/query-client";
import type { SearchBody, SearchResult } from "@/api/search";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { searchTiming } from "@/features/search/use-debounced";
import { routeTree } from "@/routeTree.gen";
import { makeMe } from "@/test-me";
import { settled } from "@/test-settle";
import "@/i18n";

const ADMIN = makeMe({ instanceAdmin: true });

const ENV_A = "11111111-1111-4111-8111-111111111111";
const ENV_B = "22222222-2222-4222-8222-222222222222";
const ENV_C = "33333333-3333-4333-8333-333333333333";

function env(id: string, name: string): EnvironmentItem {
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
    envType: "swarm",
    agentVersion: null,
    agents: null,
    engineVersion: null,
    failingServices: null,
    lastSeen: null,
    nodes: null,
    updateAvailable: false,
  } as EnvironmentItem;
}

const ENVIRONMENTS = [env(ENV_A, "alpha"), env(ENV_B, "Beta"), env(ENV_C, "prod")];

function hit(extra: Partial<SearchResult> & Pick<SearchResult, "type" | "name">): SearchResult {
  return {
    driver: "",
    envId: ENV_A,
    envName: "alpha",
    id: `${extra.type}-${extra.name}`,
    image: "",
    managed: false,
    node: "",
    nodeId: "",
    services: 0,
    size: 0,
    stack: "",
    subnet: "",
    ...extra,
  };
}

function body(
  results: SearchResult[],
  extra: Partial<Omit<SearchBody, "counts">> & { counts?: Partial<SearchBody["counts"]> } = {},
): SearchBody {
  const counts = { all: 0, container: 0, image: 0, network: 0, service: 0, stack: 0 };
  for (const r of results) counts[r.type]++;
  const { counts: over, ...rest } = extra;
  Object.assign(counts, over);
  counts.all = counts.container + counts.image + counts.network + counts.service + counts.stack;
  return {
    counts,
    notSearched: null,
    results,
    searchedAt: new Date().toISOString(),
    truncated: false,
    ...rest,
  };
}

const WEB_HITS: SearchResult[] = [
  hit({ type: "stack", name: "web-stack", id: "stk1", managed: true, services: 2 }),
  hit({ type: "service", name: "web-api", id: "svc1", image: "nginx:1.27", stack: "web-stack" }),
  hit({
    type: "service",
    name: "webhook",
    id: "svc2",
    image: "hooks:2",
    envId: ENV_B,
    envName: "Beta",
  }),
  hit({
    type: "container",
    name: "web-1",
    id: "ctr1",
    image: "nginx:1.27",
    node: "node-1",
    nodeId: "n1",
  }),
];

// The server answers for the type it was given: the other types count 0 (see SearchBody.counts).
function server(all: SearchResult[], extra: Partial<Omit<SearchBody, "counts">> = {}) {
  return (url: URL) => {
    const type = url.searchParams.get("type");
    if (!type) return Response.json(body(all, extra));
    const mine = all.filter((r) => r.type === type);
    const counts = { all: 0, container: 0, image: 0, network: 0, service: 0, stack: 0 };
    counts[type as SearchResult["type"]] = mine.length;
    return Response.json(body(mine, { ...extra, counts }));
  };
}

const searchCalls: URL[] = [];
let searchReply: (url: URL) => Response | Promise<Response> = server([]);

beforeEach(() => {
  searchCalls.length = 0;
  searchReply = server([]);
  searchTiming.debounceMs = 0;
  searchTiming.announceMs = 0;
  setCsrfToken(null);
  FakeEventSource.reset();
  vi.stubGlobal("EventSource", FakeEventSource);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/instance") {
        return Response.json({ instanceName: "Lab", needsSetup: false, rememberDays: 30 });
      }
      if (path === "/api/me") return Response.json(ADMIN);
      if (path === "/api/version") {
        return Response.json({
          commit: "",
          display: "v0.1.0",
          sourceTreeUrl: "https://example.test/tree",
          sourceUrl: "https://example.test",
          version: "v0.1.0",
        });
      }
      if (path === "/api/environments") return Response.json({ items: ENVIRONMENTS });
      if (path === "/api/me/recent-environments") return Response.json({ environmentIds: [] });
      if (path === "/api/search") {
        const url = new URL(req.url);
        searchCalls.push(url);
        return searchReply(url);
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  document.body.replaceChildren();
  vi.unstubAllGlobals();
  queryClient.clear();
  searchTiming.debounceMs = 200;
  searchTiming.announceMs = 500;
  document.title = "";
});

async function renderPage(path: string) {
  const router = createRouter({
    routeTree,
    history: createMemoryHistory({ initialEntries: [path] }),
  });
  render(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
  await screen.findByRole("heading", { level: 1, name: "Search" });
  return router;
}

const resultsTable = () => screen.findByRole("table", { name: "Search results" });
const box = () => screen.getByRole("textbox", { name: "Search" });

describe("SearchResultsPage (D-18)", () => {
  it("shows every match for ?q=web: the caption, the chips with counts and the rows", async () => {
    searchReply = server(WEB_HITS);
    await renderPage("/search?q=web");

    const table = await resultsTable();
    // Type, Name, Environment, Details; one row per match.
    expect(
      within(table)
        .getAllByRole("columnheader")
        .map((h) => h.textContent),
    ).toEqual(["Type", "Name", "Environment", "Details"]);
    expect(within(table).getAllByRole("row")).toHaveLength(1 + WEB_HITS.length);

    expect(await screen.findByText(/4 results across 3 environments\. Searched/)).toBeTruthy();
    const chips = screen.getByRole("radiogroup", { name: "Type" });
    expect(
      within(chips)
        .getAllByRole("radio")
        .map((c) => c.textContent),
    ).toEqual([
      "All (4)",
      "Stacks (1)",
      "Services (2)",
      "Containers (1)",
      "Images (0)",
      "Networks (0)",
    ]);
    expect(within(chips).getByRole("radio", { name: "All (4)" }).getAttribute("aria-checked")).toBe(
      "true",
    );

    // The input is labelled Search with the page's placeholder and holds the query.
    expect(box().getAttribute("placeholder")).toBe("Search by name, image or stack");
    expect((box() as HTMLInputElement).value).toBe("web");

    // The name is a link into the item in its own environment, with the match in a mark.
    const api = within(table).getByRole("link", { name: "web-api" });
    expect(api.getAttribute("href")).toBe(`/env/${ENV_A}/services/svc1`);
    expect(api.querySelector("mark")?.textContent).toBe("web");
    const stack = within(table).getByRole("link", { name: "web-stack" });
    expect(stack.getAttribute("href")).toBe(`/env/${ENV_A}/stacks/stk1`);
    const row = api.closest("tr");
    expect(row?.textContent).toContain("Service");
    expect(row?.textContent).toContain("alpha");
    expect(row?.textContent).toContain("nginx:1.27 in web-stack");

    expect(searchCalls).toHaveLength(1);
    expect(searchCalls[0]?.searchParams.get("q")).toBe("web");
    expect(searchCalls[0]?.searchParams.get("limit")).toBe("page");
    expect(document.title).toBe("Search · Lab");
  });

  it("puts the Services chip in the URL and shows only services, keeping every chip's count", async () => {
    searchReply = server(WEB_HITS);
    const router = await renderPage("/search?q=web");
    await resultsTable();

    fireEvent.click(screen.getByRole("radio", { name: "Services (2)" }));
    await waitFor(() => expect(router.state.location.search).toMatchObject({ type: "service" }));
    expect(router.state.location.search).toMatchObject({ q: "web" });

    await waitFor(() => {
      const table = screen.getByRole("table", { name: "Search results" });
      expect(within(table).getAllByRole("row")).toHaveLength(3);
      expect(within(table).queryByRole("link", { name: "web-stack" })).toBeNull();
    });
    // The other chips keep their counts: they come from the search over every type.
    expect(screen.getByRole("radio", { name: "Stacks (1)" })).toBeTruthy();
    expect(screen.getByRole("radio", { name: "Services (2)" }).getAttribute("aria-checked")).toBe(
      "true",
    );
    const typed = searchCalls.filter((u) => u.searchParams.get("type") === "service");
    expect(typed).toHaveLength(1);

    // Back to All: no new request, the answer is cached.
    const before = searchCalls.length;
    fireEvent.click(screen.getByRole("radio", { name: "All (4)" }));
    await waitFor(() => expect(router.state.location.search).not.toHaveProperty("type"));
    await settled(queryClient);
    expect(searchCalls).toHaveLength(before);
    expect(
      within(screen.getByRole("table", { name: "Search results" })).getAllByRole("row"),
    ).toHaveLength(1 + WEB_HITS.length);
  });

  it("follows the typing into the address and the clear button empties both", async () => {
    searchReply = server(WEB_HITS);
    const router = await renderPage("/search?q=web");
    await resultsTable();

    fireEvent.change(box(), { target: { value: "  hook " } });
    await waitFor(() => expect(router.state.location.search).toMatchObject({ q: "hook" }));
    await waitFor(() =>
      expect(searchCalls.some((u) => u.searchParams.get("q") === "hook")).toBe(true),
    );

    fireEvent.click(screen.getByRole("button", { name: "Clear search" }));
    await waitFor(() => expect(router.state.location.search).not.toHaveProperty("q"));
    expect((box() as HTMLInputElement).value).toBe("");
    expect(document.activeElement).toBe(box());
    expect(await screen.findByText("Type at least 2 characters")).toBeTruthy();
  });

  it("takes a new query from outside, such as the palette, into the box", async () => {
    searchReply = server(WEB_HITS);
    const router = await renderPage("/search?q=web");
    await resultsTable();
    await router.navigate({ to: "/search", search: { q: "nginx" } });
    await waitFor(() => expect((box() as HTMLInputElement).value).toBe("nginx"));
    await waitFor(() =>
      expect(searchCalls.some((u) => u.searchParams.get("q") === "nginx")).toBe(true),
    );
    // The box's own echo did not send the old text back to the address.
    expect(router.state.location.search).toMatchObject({ q: "nginx" });
  });
});
