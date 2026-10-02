// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { setCsrfToken } from "@/api/client";
import { SUMMARY_REFETCH_MS, summaryQuery } from "@/api/environments";
import { queryClient } from "@/api/query-client";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { routeTree } from "@/routeTree.gen";
import "@/i18n";

const ME = {
  user: { id: "u1", username: "admin", role: "admin" },
  csrfToken: "csrf-1",
  permissions: { global: [], environments: {} },
};

const LAB = {
  id: "e1",
  name: "Lab host",
  kind: "local",
  type: "standalone",
  enabled: true,
  isSelfHost: true,
  createdAt: "2026-10-01T10:00:00Z",
  engine: { name: "Docker Engine", version: "29.8.2", apiVersion: "1.54" },
  status: { state: "online" },
};

const SUMMARY = {
  environment: { id: "e1", name: "Lab host", type: "standalone", status: { state: "online" } },
  engine: {
    name: "Docker Engine",
    version: "29.8.2",
    apiVersion: "1.54",
    os: "linux",
    arch: "amd64",
    storageDriver: "overlayfs",
    ncpu: 8,
    memTotal: 17179869184,
  },
  counts: {
    containers: 7,
    containersRunning: 5,
    containersStopped: 2,
    images: 12,
    volumes: 0,
    networks: 4,
  },
  isWorker: false,
  selfContainerId: "abc123def456",
  fetchedAt: "2026-10-01T10:00:00Z",
};

let sent: string[] = [];
let environments: () => Response | Promise<Response>;
let summary: () => Response | Promise<Response>;

function stubApi() {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      sent.push(path);
      if (path === "/api/instance") {
        return Response.json({ instanceName: "Lab", needsSetup: false, rememberDays: 30 });
      }
      if (path === "/api/me") return Response.json(ME);
      if (path === "/api/version") {
        return Response.json({
          commit: "",
          display: "v0.0.0",
          sourceTreeUrl: "https://example.test/tree",
          sourceUrl: "https://example.test",
          version: "v0.0.0",
        });
      }
      if (path === "/api/environments") return environments();
      if (path === "/api/env/e1/summary") return summary();
      if (path === "/api/env/e1/events") return Response.json({ items: [] });
      return Response.json({}, { status: 404 });
    }),
  );
}

function renderAt(path: string) {
  const router = createRouter({
    routeTree,
    history: createMemoryHistory({ initialEntries: [path] }),
  });
  render(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
  return router;
}

// tile finds a count tile by its label and returns the tile element.
function tile(label: string): HTMLElement {
  const el = screen.getByText(label, { selector: "span" }).parentElement;
  if (!el) throw new Error(`no tile ${label}`);
  return el;
}

beforeEach(() => {
  sent = [];
  setCsrfToken(null);
  environments = () => Response.json({ items: [LAB] });
  summary = () => Response.json(SUMMARY);
  stubApi();
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  queryClient.clear();
  window.localStorage.clear();
  document.title = "";
});

describe("home redirect", () => {
  it("sends / to the first environment's dashboard (ENV-07)", async () => {
    const router = renderAt("/");
    await screen.findByRole("heading", { name: "Lab host" });
    expect(router.state.location.pathname).toBe("/env/e1");
  });

  it("goes to the dashboard when the first environment appears while the empty page is open", async () => {
    environments = () => Response.json({ items: [] });
    const router = renderAt("/");
    await screen.findByText("No environments yet");
    environments = () => Response.json({ items: [LAB] });
    await queryClient.invalidateQueries({ queryKey: ["environments"] });
    await screen.findByRole("heading", { name: "Lab host" });
    expect(router.state.location.pathname).toBe("/env/e1");
  });

  it("stays on the empty page when no environment exists", async () => {
    environments = () => Response.json({ items: [] });
    const router = renderAt("/");
    expect(await screen.findByText("No environments yet")).toBeTruthy();
    expect(router.state.location.pathname).toBe("/");
  });

  it("shows the landing's own error when the list cannot be read", async () => {
    environments = () => new Response("boom", { status: 500 });
    const router = renderAt("/");
    expect(
      await screen.findByText(
        "Can't reach Bayplan. Check your connection and try again.",
        {},
        { timeout: 4000 },
      ),
    ).toBeTruthy();
    expect(router.state.location.pathname).toBe("/");
  }, 8000);
});

describe("dashboard", () => {
  it("shows the header, the four tiles and the engine card from one summary request", async () => {
    renderAt("/env/e1");
    const heading = await screen.findByRole("heading", { name: "Lab host" });
    const header = heading.parentElement as HTMLElement;
    expect(within(header).getByText("Standalone")).toBeTruthy();
    expect(within(header).getByText("Online")).toBeTruthy();
    expect(within(header).getByText("Bayplan (this instance)")).toBeTruthy();

    await waitFor(() => expect(within(tile("Containers")).getByText("7")).toBeTruthy());
    expect(within(tile("Containers")).getByText("5 running · 2 stopped")).toBeTruthy();
    expect(within(tile("Images")).getByText("12")).toBeTruthy();
    expect(within(tile("Networks")).getByText("4")).toBeTruthy();
    // Zero is a number, never blank.
    expect(within(tile("Volumes")).getByText("0")).toBeTruthy();
    expect(screen.queryByText("Nodes", { selector: "span" })).toBeNull();
    expect(screen.queryByText("Services", { selector: "span" })).toBeNull();

    expect(screen.getByText("Docker engine")).toBeTruthy();
    expect(screen.getByText("29.8.2")).toBeTruthy();
    expect(screen.getByText("1.54")).toBeTruthy();
    expect(screen.getByText("linux / amd64")).toBeTruthy();
    expect(screen.getByText("overlayfs")).toBeTruthy();
    expect(screen.getByText("8")).toBeTruthy();
    expect(screen.getByText("16.0 GiB")).toBeTruthy();

    // The layout and the page share one cache entry, so there is one request.
    expect(sent.filter((p) => p === "/api/env/e1/summary")).toHaveLength(1);
    expect(document.title).toBe("Dashboard · Lab");
  });

  it("refetches the counts every 30 seconds as the polling fallback (D-57)", () => {
    expect(SUMMARY_REFETCH_MS).toBe(30_000);
    expect(summaryQuery("e1").refetchInterval).toBe(30_000);
    expect(summaryQuery("e1").queryKey).toEqual(["env", "e1", "summary"]);
  });

  it("shows Nodes and Services tiles on a swarm manager, without links", async () => {
    summary = () =>
      Response.json({
        ...SUMMARY,
        counts: { ...SUMMARY.counts, nodes: 3, services: 0 },
      });
    renderAt("/env/e1");
    await waitFor(() => expect(within(tile("Nodes")).getByText("3")).toBeTruthy());
    expect(within(tile("Services")).getByText("0")).toBeTruthy();
    // Phase 1 has no destination for any tile yet, so none is a link.
    expect(screen.queryByRole("link", { name: /Nodes|Services|Containers/ })).toBeNull();
  });

  it("shows the worker notice on a swarm worker (D-24)", async () => {
    summary = () => Response.json({ ...SUMMARY, isWorker: true });
    renderAt("/env/e1");
    expect(
      await screen.findByText(
        "This host is a swarm worker. Pin Bayplan to a manager node to manage the swarm. Showing node-local data only.",
      ),
    ).toBeTruthy();
  });

  it("badges Bayplan's own host only when the server found its container (D-27)", async () => {
    summary = () => Response.json({ ...SUMMARY, selfContainerId: undefined });
    renderAt("/env/e1");
    await screen.findByRole("heading", { name: "Lab host" });
    await waitFor(() => expect(within(tile("Images")).getByText("12")).toBeTruthy());
    expect(screen.queryByText("Bayplan (this instance)")).toBeNull();
  });

  it("does not badge another host", async () => {
    environments = () => Response.json({ items: [{ ...LAB, isSelfHost: false }] });
    renderAt("/env/e1");
    await waitFor(() => expect(within(tile("Images")).getByText("12")).toBeTruthy());
    expect(screen.queryByText("Bayplan (this instance)")).toBeNull();
  });

  it("renders a dash for what Docker does not report and lets values wrap", async () => {
    summary = () =>
      Response.json({
        ...SUMMARY,
        engine: { name: "Docker Engine", version: "29.8.2", ncpu: 0, memTotal: 0 },
      });
    renderAt("/env/e1");
    const card = (await screen.findByText("Docker engine")).closest(
      "[data-slot=card]",
    ) as HTMLElement;
    await waitFor(() => expect(within(card).getByText("29.8.2")).toBeTruthy());
    // API version, OS / architecture, storage driver, CPUs and memory.
    expect(within(card).getAllByText("—")).toHaveLength(5);
    expect(within(card).getByText("29.8.2").className).toContain("break-words");
  });

  it("shows a skeleton in place of each value while loading", async () => {
    summary = () => new Promise<Response>(() => {});
    renderAt("/env/e1");
    await screen.findByText("Containers", { selector: "span" });
    expect(tile("Containers").querySelector("[data-slot=skeleton]")).not.toBeNull();
    expect(tile("Containers").textContent).toBe("Containers");
    expect(tile("Images").querySelector("[data-slot=skeleton]")).not.toBeNull();
  });

  it("shows dashes and an inline error with Retry when the summary fails, other cards still render", async () => {
    summary = () => new Response("boom", { status: 500 });
    renderAt("/env/e1");
    expect(await screen.findByText("Couldn't load the dashboard.")).toBeTruthy();
    expect(screen.getByText("Docker may be unreachable. Try again.")).toBeTruthy();
    for (const label of ["Containers", "Images", "Volumes", "Networks"]) {
      expect(within(tile(label)).getByText("—")).toBeTruthy();
    }
    expect(screen.getByText("Docker engine")).toBeTruthy();
    // The shell around it is intact and the page is not a full-page state.
    expect(screen.getByRole("button", { name: "User menu" })).toBeTruthy();

    summary = () => Response.json(SUMMARY);
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(within(tile("Containers")).getByText("7")).toBeTruthy());
    expect(screen.queryByText("Couldn't load the dashboard.")).toBeNull();
  });

  it("marks Dashboard as the current item in the sidebar", async () => {
    renderAt("/env/e1");
    const link = await screen.findByRole("link", { name: "Dashboard" });
    expect(link.getAttribute("aria-current")).toBe("page");
  });
});

describe("events feed (ENV-07, D-57)", () => {
  beforeEach(() => {
    FakeEventSource.reset();
    vi.stubGlobal("EventSource", FakeEventSource);
  });

  it("shows the feed next to the engine card and refreshes the counts when a container event arrives", async () => {
    renderAt("/env/e1");
    await screen.findByRole("heading", { name: "Lab host" });
    expect(await screen.findByText("Recent events")).toBeTruthy();
    expect(screen.getByText("Docker engine")).toBeTruthy();
    await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));
    expect(FakeEventSource.instances[0]?.url).toBe("/api/env/e1/events/stream");
    expect(within(tile("Containers")).getByText("7")).toBeTruthy();

    summary = () =>
      Response.json({
        ...SUMMARY,
        counts: { ...SUMMARY.counts, containers: 8, containersRunning: 6 },
      });
    const before = sent.filter((p) => p === "/api/env/e1/summary").length;
    FakeEventSource.instances[0]?.open();
    FakeEventSource.instances[0]?.send({
      id: "ev1",
      time: "2026-10-02T10:00:00Z",
      type: "container",
      action: "start",
      actorId: "abc",
      name: "web-1",
      attributes: { image: "nginx" },
    });

    await waitFor(() => expect(within(tile("Containers")).getByText("8")).toBeTruthy());
    expect(sent.filter((p) => p === "/api/env/e1/summary").length).toBeGreaterThan(before);
    expect(screen.getByText("Container start")).toBeTruthy();
    expect(screen.getByText("Live")).toBeTruthy();
  });

  it("opens no stream for an environment that is offline", async () => {
    summary = () =>
      Response.json(
        { type: "environment_offline", title: "x", status: 503, reason: "unreachable" },
        { status: 503 },
      );
    renderAt("/env/e1");
    await screen.findByText("Docker isn't reachable");
    expect(FakeEventSource.instances).toHaveLength(0);
  });
});
