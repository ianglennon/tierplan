// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { EnvironmentItem } from "@/api/environments";
import { queryClient } from "@/api/query-client";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { routeTree } from "@/routeTree.gen";
import "@/i18n";

function permissions(...perms: string[]) {
  return { global: perms.filter((p) => p === "environments.create"), environments: { "*": perms } };
}

const ADMIN = {
  user: { id: "u1", username: "admin", role: "admin" },
  csrfToken: "csrf-1",
  permissions: permissions("environments.read", "environments.create", "environments.token"),
};

function env(id: string, name: string, state: EnvironmentItem["status"]["state"], extra = {}) {
  return {
    id,
    name,
    kind: "agent",
    enabled: true,
    isSelfHost: false,
    isLocalSocket: false,
    createdAt: "2026-10-01T10:00:00Z",
    engine: {},
    status: { state },
    agentVersion: null,
    agents: null,
    engineVersion: null,
    failingServices: null,
    lastSeen: null,
    nodes: null,
    updateAvailable: false,
    ...extra,
  };
}

const ONLINE = env("e1", "nas", "online", {
  envType: "standalone",
  nodes: { ready: 1, total: 1 },
  engineVersion: "28.5.1",
});
const OFFLINE = env("e2", "edge", "offline", {
  envType: "standalone",
  status: { state: "offline", reason: "agent_offline", since: "2026-10-03T09:00:00Z" },
});

let me: Record<string, unknown>;
let list: () => Response | Promise<Response>;
let sent: string[];

beforeEach(() => {
  me = ADMIN;
  sent = [];
  list = () => Response.json({ items: [ONLINE, OFFLINE] });
  setCsrfToken(null);
  FakeEventSource.reset();
  vi.stubGlobal("EventSource", FakeEventSource);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      sent.push(`${req.method} ${path}`);
      if (path === "/api/instance") {
        return Response.json({ instanceName: "Lab", needsSetup: false, rememberDays: 30 });
      }
      if (path === "/api/me") return Response.json(me);
      if (path === "/api/version") {
        return Response.json({
          commit: "",
          display: "v0.1.0",
          sourceTreeUrl: "https://example.test/tree",
          sourceUrl: "https://example.test",
          version: "v0.1.0",
        });
      }
      if (path === "/api/local-host") {
        return Response.json({
          socketPath: "/var/run/docker.sock",
          detected: false,
          managed: false,
        });
      }
      if (path === "/api/environments") return list();
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  queryClient.clear();
  document.title = "";
});

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

const names = () =>
  within(screen.getByRole("table"))
    .getAllByRole("link")
    .map((a) => a.textContent);
const stream = () => {
  const s = FakeEventSource.instances.find((i) => i.url === "/api/environments/stream");
  if (!s) throw new Error("the health stream is not open");
  return s;
};

describe("/environments", () => {
  it("shows the title, the count and the environments, problems first", async () => {
    renderAt("/environments");
    expect(await screen.findByRole("heading", { level: 1, name: "Environments" })).toBeTruthy();
    await screen.findByRole("table");
    expect(screen.getByText("2 environments")).toBeTruthy();
    expect(names()).toEqual(["edge", "nas"]);
    expect(document.title).toBe("Environments · Lab");
  });

  it("reads one environment as '1 environment'", async () => {
    list = () => Response.json({ items: [ONLINE] });
    renderAt("/environments");
    expect(await screen.findByText("1 environment")).toBeTruthy();
  });

  it("writes the sort to the URL: ascending first, then descending", async () => {
    const router = renderAt("/environments");
    await screen.findByRole("table");
    fireEvent.click(screen.getByRole("button", { name: "Sort by Name" }));
    await waitFor(() =>
      expect(router.state.location.search).toMatchObject({ sort: "name", dir: "asc" }),
    );
    expect(router.state.location.searchStr).toContain("sort=name");
    expect(router.state.location.searchStr).toContain("dir=asc");
    expect(names()).toEqual(["edge", "nas"]);
    fireEvent.click(screen.getByRole("button", { name: "Sort by Name" }));
    await waitFor(() => expect(router.state.location.search).toMatchObject({ dir: "desc" }));
    expect(names()).toEqual(["nas", "edge"]);
  });

  it("reads the sort from the URL", async () => {
    renderAt("/environments?sort=name&dir=desc");
    await screen.findByRole("link", { name: "nas" });
    expect(names()).toEqual(["nas", "edge"]);
  });

  it("ignores a sort it does not know and shows problems first", async () => {
    renderAt("/environments?sort=bogus&dir=sideways");
    await screen.findByRole("link", { name: "nas" });
    expect(names()).toEqual(["edge", "nas"]);
  });

  it("shows six skeleton rows while loading, and never the empty state", async () => {
    list = () => new Promise<Response>(() => {});
    renderAt("/environments");
    const table = await screen.findByRole("table");
    expect(table.getAttribute("aria-busy")).toBe("true");
    expect(table.querySelectorAll("tbody tr")).toHaveLength(6);
    expect(screen.queryByText("No environments yet")).toBeNull();
  });

  it("shows the empty state with Add environment and the local host opt-in for someone who can create", async () => {
    list = () => Response.json({ items: [] });
    renderAt("/environments");
    expect(await screen.findByText("No environments yet")).toBeTruthy();
    expect(
      screen.getByText(
        "Add a swarm or a standalone Docker host. Bayplan gives you one command that installs a small agent on it. The host needs no open inbound port.",
      ),
    ).toBeTruthy();
    expect(screen.getAllByRole("button", { name: "Add environment" }).length).toBeGreaterThan(0);
    expect(await screen.findByRole("heading", { name: "No Docker socket found" })).toBeTruthy();
  });

  it("tells someone who cannot create to ask an admin", async () => {
    me = { ...ADMIN, permissions: permissions("environments.read") };
    list = () => Response.json({ items: [] });
    renderAt("/environments");
    expect(await screen.findByText("Ask an admin to add an environment.")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Add environment" })).toBeNull();
  });

  it("shows Couldn't load environments with Retry, and Retry reads the list again", {
    timeout: 8000,
  }, async () => {
    list = () => new Response("boom", { status: 500 });
    renderAt("/environments");
    // The client retries a failed read once before it gives up.
    expect(
      await screen.findByText("Couldn't load environments.", {}, { timeout: 4000 }),
    ).toBeTruthy();
    expect(screen.getByText("Check the connection and try again.")).toBeTruthy();
    list = () => Response.json({ items: [ONLINE] });
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByRole("table")).toBeTruthy();
    expect(screen.queryByText("Couldn't load environments.")).toBeNull();
  });

  it("puts Add environment in the header for someone who can create, and leaves it out otherwise", async () => {
    renderAt("/environments");
    await screen.findByRole("table");
    expect(screen.getByRole("button", { name: "Add environment" })).toBeTruthy();
    cleanup();
    me = { ...ADMIN, permissions: permissions("environments.read") };
    queryClient.clear();
    renderAt("/environments");
    await screen.findByRole("table");
    expect(screen.queryByRole("button", { name: "Add environment" })).toBeNull();
  });

  it("shows No permission to someone who may read no environment", async () => {
    me = { ...ADMIN, permissions: { global: [], environments: {} } };
    renderAt("/environments");
    expect(
      await screen.findByRole("heading", { name: "You don't have permission to view this page" }),
    ).toBeTruthy();
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("shows Live once the stream opens, Reconnecting… while it retries, and nothing once polling", async () => {
    renderAt("/environments");
    await screen.findByRole("table");
    expect(screen.queryByText("Live")).toBeNull();
    await act(async () => stream().open());
    expect(screen.getByText("Live")).toBeTruthy();
    await act(async () => stream().fail());
    expect(screen.getByText("Reconnecting…")).toBeTruthy();
    await act(async () => stream().fail(true));
    expect(screen.queryByText("Reconnecting…")).toBeNull();
    expect(screen.queryByText("Live")).toBeNull();
  });

  it("updates a row from the stream without reading the list again", async () => {
    renderAt("/environments");
    await screen.findByRole("link", { name: "nas" });
    await act(async () => stream().open());
    const reads = sent.filter((s) => s === "GET /api/environments").length;
    await act(async () => {
      stream().sendFrame(
        "env.status",
        {
          envId: "e1",
          at: "2026-10-03T10:00:00Z",
          environment: { ...ONLINE, status: { state: "degraded", reason: "clock_skew" } },
        },
        "3",
      );
    });
    // The cache notifies its observers on the next tick.
    await waitFor(() => {
      const row = screen
        .getAllByRole("row")
        .find((r) => within(r).queryByText("nas")) as HTMLElement;
      expect(within(row).getByText("Degraded")).toBeTruthy();
    });
    expect(sent.filter((s) => s === "GET /api/environments")).toHaveLength(reads);
  });
});

describe("the sidebar's Environments item", () => {
  it("sits above the environment's own sections and is active only on /environments", async () => {
    renderAt("/environments");
    await screen.findByRole("table");
    const link = screen.getByRole("link", { name: "Environments" });
    expect(link.getAttribute("aria-current")).toBe("page");
    cleanup();

    renderAt("/env/e1");
    const dashboard = await screen.findByRole("link", { name: "Dashboard" });
    const other = screen.getByRole("link", { name: "Environments" });
    expect(other.getAttribute("aria-current")).toBeNull();
    expect(
      other.compareDocumentPosition(dashboard) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
  });

  it("is left out for someone who may read no environment", async () => {
    me = { ...ADMIN, permissions: { global: [], environments: {} } };
    renderAt("/environments");
    await screen.findByRole("heading", { name: "You don't have permission to view this page" });
    expect(screen.queryByRole("link", { name: "Environments" })).toBeNull();
  });
});
