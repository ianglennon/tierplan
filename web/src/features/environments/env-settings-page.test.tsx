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
import { splitPermissions } from "@/test-permissions";

const ALL = [
  "environments.read",
  "environments.manage",
  "environments.token",
  "environments.revoke",
  "environments.delete",
];

// A permission named in either list lands where the server puts it (global scope
// in `global`), so a test says what the caller holds without caring which list.
function me(envPerms: string[], globalPerms: string[] = ["settings.read"]) {
  return {
    user: { id: "u1", username: "admin", role: "admin" },
    csrfToken: "csrf-1",
    permissions: splitPermissions(envPerms, globalPerms),
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

const NODE = {
  id: "a1",
  node: "mgr-1",
  role: "manager",
  status: "active",
  blocked: false,
  connected: true,
  agentVersion: "0.1.0",
  engineVersion: "28.5.1",
  certFingerprint: "ab".repeat(32),
  certNotAfter: "2027-01-01T00:00:00Z",
  clockOffsetSeconds: 1,
  lastSeen: "2026-10-03T11:59:00Z",
  readOnly: false,
  selfUpdate: true,
};

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
      if (path === "/api/env/e2/agents") return Response.json({ items: [NODE] });
      if (path === "/api/environments/e2/adopt-cluster") {
        return Response.json({ clusterId: "x3k2m9t0q4w5e6r7t8y9u0i1o" });
      }
      if (path === "/api/environments/e2/agents/a1/revoke")
        return new Response(null, { status: 204 });
      if (path === "/api/environments/e2/fold-local" && req.method === "POST") {
        // The local environment is retired and nothing offers a fold any more.
        items = [{ ...AGENT_ENV, foldOffered: undefined }];
        return Response.json({ ...AGENT_ENV, foldOffered: undefined });
      }
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
  it("links every row to its page, adds Manage to an agent environment, and keeps Rename and Remove host on the local socket row", async () => {
    renderAt("/settings/environments");
    const link = await screen.findByRole("link", { name: "edge" });
    expect(link.getAttribute("href")).toBe("/settings/environments/e2");
    expect(screen.getByRole("link", { name: "Manage edge" }).getAttribute("href")).toBe(
      "/settings/environments/e2",
    );
    // The local host's row opens its page by its name and keeps Phase 1's actions.
    expect(screen.getByRole("link", { name: "lab-1" }).getAttribute("href")).toBe(
      "/settings/environments/e1",
    );
    expect(screen.getByRole("button", { name: "Rename lab-1" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Remove host lab-1" })).toBeTruthy();
    expect(screen.queryByRole("link", { name: "Manage lab-1" })).toBeNull();
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

  it("renders General, Connection, Nodes and agents, Join token, Agent updates and Remove environment in that order for a caller who holds every permission", async () => {
    renderAt("/settings/environments/e2");
    await screen.findByRole("heading", { level: 3, name: "General" });
    await screen.findByRole("heading", { level: 3, name: "Join token" });
    expect(headings()).toEqual([
      "General",
      "Connection",
      "Nodes and agents",
      "Join token",
      "Agent updates",
      "Remove environment",
    ]);
  });

  it("makes Remove environment the last card, with the anchor remove, and scrolls to it for #remove", async () => {
    const scroll = vi.fn();
    const original = Element.prototype.scrollIntoView;
    Element.prototype.scrollIntoView = scroll;
    try {
      renderAt("/settings/environments/e2#remove");
      const heading = await screen.findByRole("heading", { level: 3, name: "Remove environment" });
      const section = heading.closest("section");
      expect(section?.id).toBe("remove");
      expect(section?.nextElementSibling).toBeNull();
      await waitFor(() => expect(scroll).toHaveBeenCalled());
      expect(scroll.mock.contexts.at(-1)).toBe(section);
    } finally {
      Element.prototype.scrollIntoView = original;
    }
  });

  it("does not scroll to Remove environment without the anchor", async () => {
    const scroll = vi.fn();
    const original = Element.prototype.scrollIntoView;
    Element.prototype.scrollIntoView = scroll;
    try {
      renderAt("/settings/environments/e2");
      await screen.findByRole("heading", { level: 3, name: "Remove environment" });
      expect(scroll).not.toHaveBeenCalled();
    } finally {
      Element.prototype.scrollIntoView = original;
    }
  });

  it("hides Remove environment without environments.delete, and Agent updates without environments.manage or on the local socket", async () => {
    current = me(["environments.read", "environments.manage", "environments.token"]);
    renderAt("/settings/environments/e2");
    await screen.findByRole("heading", { level: 3, name: "Agent updates" });
    expect(screen.queryByRole("heading", { name: "Remove environment" })).toBeNull();
    cleanup();

    current = me(["environments.read", "environments.token", "environments.delete"]);
    renderAt("/settings/environments/e2");
    await screen.findByRole("heading", { level: 3, name: "Remove environment" });
    expect(screen.queryByRole("heading", { name: "Agent updates" })).toBeNull();
    cleanup();

    // The host Bayplan runs on has no agents to update.
    current = me(ALL);
    renderAt("/settings/environments/e1");
    await screen.findByRole("heading", { level: 3, name: "Remove environment" });
    expect(screen.queryByRole("heading", { name: "Agent updates" })).toBeNull();
    expect(screen.queryByRole("heading", { name: "Join token" })).toBeNull();
  });

  it("keeps Phase 1's Remove host wording for the local socket, with no uninstall box and no manual command", async () => {
    renderAt("/settings/environments/e1");
    const heading = await screen.findByRole("heading", { level: 3, name: "Remove environment" });
    const button = within(heading.closest("section") as HTMLElement).getByRole("button", {
      name: "Remove host",
    });
    fireEvent.click(button);
    const dialog = await screen.findByRole("alertdialog", { name: "Remove lab-1?" });
    expect(within(dialog).getByRole("button", { name: "Remove host" })).toBeTruthy();
    expect(within(dialog).queryByRole("checkbox")).toBeNull();
    expect(within(dialog).queryByText("Manual cleanup command")).toBeNull();
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

  it("offers Adopt new cluster on the Connection card while the cluster is changed, and not otherwise", async () => {
    items = [
      env({
        status: {
          state: "cluster_changed",
          reason: "cluster_changed",
          since: "2026-10-03T09:00:00Z",
        },
        pendingClusterId: "x3k2m9t0q4w5e6r7t8y9u0i1o",
      }),
      LOCAL,
    ];
    renderAt("/settings/environments/e2");
    const card = (await screen.findByRole("heading", { name: "Connection" })).closest("section");
    const button = await within(card as HTMLElement).findByRole("button", {
      name: "Adopt new cluster",
    });
    fireEvent.click(button);
    const dialog = await screen.findByRole("dialog", { name: "Adopt the new cluster?" });
    expect(dialog.textContent).toContain(
      "edge will be bound to the swarm that now reports cluster ID x3k2m9t0q4w5e6r7t8y9u0i1o",
    );
    fireEvent.click(within(dialog).getByRole("button", { name: "Adopt new cluster" }));
    await waitFor(() => expect(seen).toContain("POST /api/environments/e2/adopt-cluster"));
  });

  it("does not offer Adopt new cluster on an online environment or to someone without environments.manage", async () => {
    renderAt("/settings/environments/e2");
    const card = (await screen.findByRole("heading", { name: "Connection" })).closest("section");
    expect(
      within(card as HTMLElement).queryByRole("button", { name: "Adopt new cluster" }),
    ).toBeNull();
    cleanup();

    items = [env({ status: { state: "cluster_changed", reason: "cluster_changed" } }), LOCAL];
    current = me(["environments.read", "environments.token"], ["settings.read"]);
    renderAt("/settings/environments/e2");
    const card2 = (await screen.findByRole("heading", { name: "Connection" })).closest("section");
    expect(
      await within(card2 as HTMLElement).findByText("This swarm was re-initialised."),
    ).toBeTruthy();
    expect(
      within(card2 as HTMLElement).queryByRole("button", { name: "Adopt new cluster" }),
    ).toBeNull();
  });

  it("shows only the check command that fits a swarm, not the standalone one too", async () => {
    items = [DEGRADED, LOCAL];
    renderAt("/settings/environments/e2");
    const card = (await screen.findByRole("heading", { name: "Connection" })).closest("section");
    const within_ = within(card as HTMLElement);
    expect(await within_.findByText("docker service ps bayplan-agent")).toBeTruthy();
    expect(within_.queryByText("docker ps --filter name=bayplan-agent")).toBeNull();
  });

  it("explains why the local Docker socket is offline in words", async () => {
    items = [
      AGENT_ENV,
      {
        ...LOCAL,
        status: { state: "offline", reason: "unreachable", since: "2026-10-03T09:00:00Z" },
      },
    ];
    renderAt("/settings/environments/e1");
    const card = (await screen.findByRole("heading", { name: "Connection" })).closest("section");
    const within_ = within(card as HTMLElement);
    expect(await within_.findByText(/Docker isn't answering on the local socket\./)).toBeTruthy();
    expect(within_.getByText(/mounted into Bayplan/)).toBeTruthy();
  });

  it("hides the Join token card and never reads the token without environments.token", async () => {
    current = me(["environments.read", "environments.manage", "environments.revoke"]);
    renderAt("/settings/environments/e2");
    await screen.findByRole("heading", { level: 3, name: "General" });
    expect(headings()).toEqual(["General", "Connection", "Nodes and agents", "Agent updates"]);
    expect(seen).not.toContain("GET /api/environments/e2/token");
  });

  it("hides Rename and the General card without environments.manage", async () => {
    current = me(["environments.read", "environments.token"]);
    renderAt("/settings/environments/e2");
    await screen.findByRole("heading", { level: 3, name: "Join token" });
    expect(headings()).toEqual(["Connection", "Nodes and agents", "Join token"]);
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
    await screen.findByRole("heading", { level: 3, name: "Remove environment" });
    expect(headings()).toEqual(["General", "Connection", "Nodes and agents", "Remove environment"]);
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

  it("puts the Local host card between Agent updates and Remove environment while a fold is offered, and not otherwise", async () => {
    items = [env({ foldOffered: true }), { ...LOCAL, foldOffered: true }];
    renderAt("/settings/environments/e2");
    await screen.findByRole("heading", { level: 3, name: "Local host" });
    expect(headings()).toEqual([
      "General",
      "Connection",
      "Nodes and agents",
      "Join token",
      "Agent updates",
      "Local host",
      "Remove environment",
    ]);
    expect(screen.getByRole("button", { name: "Fold into edge" })).toBeTruthy();
    cleanup();
    // The first list is what the cache would show until the second one arrives.
    queryClient.clear();

    items = [AGENT_ENV, LOCAL];
    renderAt("/settings/environments/e2");
    await screen.findByRole("heading", { level: 3, name: "Remove environment" });
    expect(screen.queryByRole("heading", { level: 3, name: "Local host" })).toBeNull();
  });

  it("hides the Local host card from someone without environments.manage", async () => {
    items = [env({ foldOffered: true }), { ...LOCAL, foldOffered: true }];
    current = me(["environments.read", "environments.delete"], ["settings.read"]);
    renderAt("/settings/environments/e2");
    await screen.findByRole("heading", { level: 3, name: "Remove environment" });
    expect(screen.queryByRole("heading", { level: 3, name: "Local host" })).toBeNull();
  });

  it("offers the fold on the local host's own page too, and after it moves to the swarm's page", async () => {
    items = [env({ foldOffered: true }), { ...LOCAL, foldOffered: true }];
    const router = renderAt("/settings/environments/e1");
    fireEvent.click(await screen.findByRole("button", { name: "Fold into edge" }));
    const dialog = await screen.findByRole("dialog", { name: "Fold this host into edge?" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Fold into edge" }));
    await waitFor(() => expect(seen).toContain("POST /api/environments/e2/fold-local"));
    await waitFor(() => expect(router.state.location.pathname).toBe("/settings/environments/e2"));
    expect(await screen.findByRole("heading", { level: 2, name: "edge" })).toBeTruthy();
  });

  it("moves focus to the page title after a fold on the swarm's own page, since the card that had it is gone", async () => {
    items = [env({ foldOffered: true }), { ...LOCAL, foldOffered: true }];
    renderAt("/settings/environments/e2");
    fireEvent.click(await screen.findByRole("button", { name: "Fold into edge" }));
    const dialog = await screen.findByRole("dialog", { name: "Fold this host into edge?" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Fold into edge" }));
    await waitFor(() =>
      expect(screen.queryByRole("heading", { level: 3, name: "Local host" })).toBeNull(),
    );
    await waitFor(() =>
      expect(document.activeElement).toBe(screen.getByRole("heading", { level: 2, name: "edge" })),
    );
  });

  it("lists the environment's nodes in the Nodes and agents card", async () => {
    renderAt("/settings/environments/e2");
    const card = (await screen.findByRole("heading", { name: "Nodes and agents" })).closest(
      "section",
    );
    expect(await within(card as HTMLElement).findByText("mgr-1")).toBeTruthy();
    expect(within(card as HTMLElement).getByRole("table")).toBeTruthy();
  });

  it("opens the install dialog in rotate mode from the toast offered after a revoke", async () => {
    renderAt("/settings/environments/e2");
    const trigger = await screen.findByRole("button", { name: "Actions for mgr-1" });
    fireEvent.keyDown(trigger, { key: "Enter" });
    fireEvent.click(await screen.findByRole("menuitem", { name: "Revoke node" }));
    const alertDialog = await screen.findByRole("alertdialog");
    fireEvent.click(within(alertDialog).getByRole("button", { name: "Revoke node" }));
    expect(await screen.findByText("mgr-1 was revoked.")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Rotate join token" }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByRole("heading", { name: "Show install command" })).toBeTruthy();
    expect(within(dialog).getByRole("button", { name: "Generate new token" })).toBeTruthy();
    // Nothing is rotated by opening it.
    expect(seen).not.toContain("POST /api/environments/e2/token");
  });
});
