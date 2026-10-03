// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { queryClient } from "@/api/query-client";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { routeTree } from "@/routeTree.gen";
import "@/i18n";

const ALL = [
  "environments.read",
  "environments.manage",
  "environments.token",
  "environments.revoke",
];

function me(envPerms: string[], globalPerms: string[] = ["settings.read", "environments.manage"]) {
  return {
    user: { id: "u1", username: "admin", role: "admin" },
    csrfToken: "csrf-1",
    permissions: { global: globalPerms, environments: { "*": envPerms } },
  };
}

function env(extra: Record<string, unknown>) {
  return {
    id: "e2",
    name: "edge",
    kind: "agent",
    enabled: true,
    isSelfHost: false,
    isLocalSocket: false,
    createdAt: "2026-10-01T10:00:00Z",
    engine: { name: "Docker Engine", version: "28.5.1", apiVersion: "1.50" },
    status: { state: "online" },
    envType: "swarm",
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

const AGENT_ENV = env({});
const DEGRADED = env({
  status: {
    state: "degraded",
    since: "2026-10-03T09:00:00Z",
    issues: [
      { code: "agent_offline", node: "db-2", since: "2026-10-03T09:00:00Z" },
      { code: "clock_skew", node: "db-3", since: "2026-10-03T09:30:00Z" },
    ],
  },
});
const LOCAL = env({
  id: "e1",
  name: "lab-1",
  kind: "local",
  isSelfHost: true,
  isLocalSocket: true,
  envType: "standalone",
});

let current: Record<string, unknown>;
let items: Record<string, unknown>[];
let seen: string[];

beforeEach(() => {
  current = me(ALL);
  items = [AGENT_ENV, LOCAL];
  seen = [];
  setCsrfToken(null);
  FakeEventSource.reset();
  vi.stubGlobal("EventSource", FakeEventSource);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      seen.push(`${req.method} ${path}`);
      if (path === "/api/instance") {
        return Response.json({ instanceName: "Lab", needsSetup: false, rememberDays: 30 });
      }
      if (path === "/api/me") return Response.json(current);
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
        return Response.json({ socketPath: "/var/run/docker.sock", detected: true, managed: true });
      }
      if (path === "/api/environments") return Response.json({ items });
      if (path === "/api/settings") {
        return Response.json({
          agentEndpoint: "wss://bayplan.example.com/agent/connect",
          agentUrl: "",
          agentUrlLocked: false,
          instanceName: "Lab",
          publicUrl: "https://bayplan.example.com",
          publicUrlLocked: false,
        });
      }
      if (path === "/api/environments/e2/token") {
        return Response.json({
          generation: 2,
          createdAt: "2026-10-03T09:00:00Z",
          createdBy: "admin",
          disabled: false,
        });
      }
      if (path === "/api/env/e2/agents") return Response.json({ items: [] });
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  queryClient.clear();
  setCsrfToken(null);
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

const headings = () => screen.getAllByRole("heading", { level: 3 }).map((h) => h.textContent);

describe("/settings/environments (the list)", () => {
  it("links an agent environment's row to its page and keeps Rename and Remove host on the local socket row", async () => {
    renderAt("/settings/environments");
    const link = await screen.findByRole("link", { name: "edge" });
    expect(link.getAttribute("href")).toBe("/settings/environments/e2");
    expect(screen.getByRole("link", { name: "Manage edge" }).getAttribute("href")).toBe(
      "/settings/environments/e2",
    );
    // The local host has nothing to open: Phase 1's actions stay on it.
    expect(screen.getByRole("button", { name: "Rename lab-1" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Remove host lab-1" })).toBeTruthy();
    expect(screen.queryByRole("link", { name: "lab-1" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Rename edge" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Remove host edge" })).toBeNull();
  });
});

describe("/settings/environments/$envId", () => {
  it("shows Settings / Environments / {name} as the breadcrumb and the name as the title", async () => {
    renderAt("/settings/environments/e2");
    const nav = await screen.findByRole("navigation", { name: "Breadcrumb" });
    const crumbs = within(nav)
      .getAllByRole("listitem")
      .map((li) => li.textContent);
    expect(crumbs).toEqual(["Settings", "Environments", "edge"]);
    expect(within(nav).getByRole("link", { name: "Environments" }).getAttribute("href")).toBe(
      "/settings/environments",
    );
    expect(within(nav).getByRole("link", { name: "Settings" })).toBeTruthy();
    expect(within(nav).getByText("edge").getAttribute("aria-current")).toBe("page");
    expect(screen.getByRole("heading", { level: 2, name: "edge" })).toBeTruthy();
    expect(document.title).toBe("edge · Lab");
    // The Settings tab it lives under stays marked as the current one.
    expect(
      screen.getByRole("tab", { name: "Environments" }).getAttribute("data-active"),
    ).not.toBeNull();
  });

  it("renders General, Connection and Join token in that order for a caller who holds all three permissions", async () => {
    renderAt("/settings/environments/e2");
    await screen.findByRole("heading", { level: 3, name: "General" });
    await screen.findByRole("heading", { level: 3, name: "Join token" });
    expect(headings()).toEqual(["General", "Connection", "Join token"]);
  });

  it("shows the health dot with its label and the agent URL in use on the Connection card", async () => {
    renderAt("/settings/environments/e2");
    const card = (await screen.findByRole("heading", { name: "Connection" })).closest("section");
    expect(card).toBeTruthy();
    const within_ = within(card as HTMLElement);
    expect(within_.getByText("Online")).toBeTruthy();
    expect(within_.getByText("Connects to")).toBeTruthy();
    expect(await within_.findByText("wss://bayplan.example.com/agent/connect")).toBeTruthy();
    // Nothing to explain while it is online.
    expect(within_.queryByText(/isn't connected/)).toBeNull();
  });

  it("explains the active reason with ReasonText and counts the others when degraded", async () => {
    items = [DEGRADED, LOCAL];
    renderAt("/settings/environments/e2");
    const card = (await screen.findByRole("heading", { name: "Connection" })).closest("section");
    const within_ = within(card as HTMLElement);
    expect(await within_.findByText("Degraded")).toBeTruthy();
    expect(within_.getByText(/The agent on db-2 isn't connected\./)).toBeTruthy();
    expect(within_.getByText("and 1 more issue")).toBeTruthy();
  });

  it("hides the Join token card and never reads the token without environments.token", async () => {
    current = me(["environments.read", "environments.manage", "environments.revoke"]);
    renderAt("/settings/environments/e2");
    await screen.findByRole("heading", { level: 3, name: "General" });
    expect(headings()).toEqual(["General", "Connection"]);
    expect(seen).not.toContain("GET /api/environments/e2/token");
  });

  it("hides Rename and the General card without environments.manage", async () => {
    current = me(["environments.read", "environments.token"]);
    renderAt("/settings/environments/e2");
    await screen.findByRole("heading", { level: 3, name: "Join token" });
    expect(headings()).toEqual(["Connection", "Join token"]);
    expect(screen.queryByRole("button", { name: /^Rename/ })).toBeNull();
  });

  it("leaves out the agent URL, and never reads the settings, without settings.read", async () => {
    current = me(ALL, ["environments.manage"]);
    renderAt("/settings/environments/e2");
    const card = (await screen.findByRole("heading", { name: "Connection" })).closest("section");
    expect(within(card as HTMLElement).getByText("Online")).toBeTruthy();
    expect(within(card as HTMLElement).queryByText("Connects to")).toBeNull();
    expect(seen).not.toContain("GET /api/settings");
  });

  it("shows no permission when the caller holds none of the page's permissions", async () => {
    current = me(["environments.read"], []);
    renderAt("/settings/environments/e2");
    expect(await screen.findByText("You don't have permission to view this page")).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "General" })).toBeNull();
  });

  it("opens the Phase 1 rename dialog from Rename", async () => {
    renderAt("/settings/environments/e2");
    fireEvent.click(await screen.findByRole("button", { name: "Rename edge" }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByRole("heading", { name: "Rename environment" })).toBeTruthy();
    expect((within(dialog).getByLabelText("Name") as HTMLInputElement).value).toBe("edge");
  });

  it("says the environment was not found for an unknown ID", async () => {
    renderAt("/settings/environments/nope");
    expect(await screen.findByText("Environment not found")).toBeTruthy();
    expect(screen.getByRole("link", { name: "Back to environments" }).getAttribute("href")).toBe(
      "/settings/environments",
    );
  });

  it("has no Join token card, no Connects to and no token read for the local socket host", async () => {
    renderAt("/settings/environments/e1");
    await screen.findByRole("heading", { level: 3, name: "General" });
    expect(headings()).toEqual(["General", "Connection"]);
    expect(screen.queryByText("Connects to")).toBeNull();
    await waitFor(() => expect(seen).toContain("GET /api/environments"));
    expect(seen).not.toContain("GET /api/environments/e1/token");
  });

  it("shows a loading status while the list loads", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (req: Request) => {
        const path = new URL(req.url).pathname;
        if (path === "/api/instance") {
          return Response.json({ instanceName: "Lab", needsSetup: false, rememberDays: 30 });
        }
        if (path === "/api/me") return Response.json(current);
        if (path === "/api/environments") return new Promise<Response>(() => {});
        return Response.json({}, { status: 404 });
      }),
    );
    renderAt("/settings/environments/e2");
    expect(await screen.findByText("Loading the environment…")).toBeTruthy();
  });
});
