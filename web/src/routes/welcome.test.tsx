// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { queryClient } from "@/api/query-client";
import { routeTree } from "@/routeTree.gen";
import "@/i18n";
import { makeMe } from "@/test-me";
import { settled } from "@/test-settle";

const ADMIN = makeMe({
  instanceAdmin: true,
  permissions: { global: ["environments.create"], environments: { "*": ["environments.read"] } },
});
const VIEWER = {
  ...ADMIN,
  permissions: { global: [], environments: { "*": ["environments.read"] } },
};

const DETECTED = {
  socketPath: "/var/run/docker.sock",
  detected: true,
  managed: false,
  probe: {
    probedAt: "2026-10-01T10:00:00Z",
    reachable: true,
    supported: true,
    engineName: "Docker Engine",
    engineVersion: "29.8.2",
    apiVersion: "1.56",
    hostname: "lab-1",
    type: "standalone",
  },
};

interface Call {
  path: string;
  method: string;
  csrf: string | null;
  body?: Record<string, unknown>;
}

// The host once it is an environment, as the list returns it.
const ADDED = {
  id: "e1",
  name: "lab-1",
  kind: "local",
  enabled: true,
  isSelfHost: true,
  isLocalSocket: true,
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

let calls: Call[];
let me: Record<string, unknown> | null;
let needsSetup: boolean;
let localHost: Record<string, unknown>;
let environments: unknown[];

beforeEach(() => {
  calls = [];
  me = ADMIN;
  needsSetup = false;
  localHost = DETECTED;
  environments = [];
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      const call: Call = { path, method: req.method, csrf: req.headers.get("X-CSRF-Token") };
      if (req.method === "POST") call.body = await req.clone().json();
      calls.push(call);
      if (path === "/api/instance") {
        return Response.json({ instanceName: "Lab", needsSetup, rememberDays: 30 });
      }
      if (path === "/api/me") {
        return me ? Response.json(me) : Response.json({ status: 401 }, { status: 401 });
      }
      if (path === "/api/local-host") return Response.json(localHost);
      if (path === "/api/environments" && req.method === "POST") {
        // Adding the host makes it an environment.
        environments = [{ ...ADDED }];
        localHost = { ...DETECTED, managed: true };
        return Response.json({ id: "e1" }, { status: 201 });
      }
      if (path === "/api/environments") return Response.json({ items: environments });
      if (path === "/api/version") {
        return Response.json({
          commit: "",
          display: "v0.1.0",
          sourceTreeUrl: "https://example.test/tree",
          sourceUrl: "https://example.test",
          version: "v0.1.0",
        });
      }
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

const posts = () => calls.filter((c) => c.method === "POST");

describe("/welcome (wizard step 4)", () => {
  it("shows step 4 of 4 with the detected engine and adds nothing", async () => {
    renderAt("/welcome");

    expect(await screen.findByRole("heading", { name: "Manage this Docker host?" })).toBeTruthy();
    expect(screen.getAllByText("Step 4 of 4").length).toBeGreaterThan(0);
    expect(screen.getByRole("heading", { level: 1, name: "Coaming" })).toBeTruthy();
    expect(screen.getByText("Docker Engine 29.8.2")).toBeTruthy();
    expect(screen.getByText("Standalone")).toBeTruthy();
    expect(document.title).toBe("Welcome · Lab");
    await settled(queryClient);
    expect(posts()).toEqual([]);
  });

  it("adds the host with the session's CSRF token on Manage this host, then lands on its dashboard", async () => {
    const router = renderAt("/welcome");
    await screen.findByRole("heading", { name: "Manage this Docker host?" });

    fireEvent.click(screen.getByRole("button", { name: "Manage this host" }));

    // Home sends a user with no last-used environment to the overview, which
    // now lists the host that was just added.
    await waitFor(() => expect(router.state.location.pathname).toBe("/environments"));
    expect(await screen.findByRole("link", { name: "lab-1" })).toBeTruthy();
    expect(posts()).toEqual([
      { path: "/api/environments", method: "POST", csrf: "csrf-1", body: { kind: "local" } },
    ]);
  });

  it("goes home on Skip for now without adding anything", async () => {
    const router = renderAt("/welcome");
    await screen.findByRole("heading", { name: "Manage this Docker host?" });

    fireEvent.click(screen.getByRole("button", { name: "Skip for now" }));

    await waitFor(() => expect(router.state.location.pathname).toBe("/environments"));
    expect(posts()).toEqual([]);
  });

  it("sends a visitor who is not signed in to /login", async () => {
    me = null;
    const router = renderAt("/welcome");
    await waitFor(() => expect(router.state.location.pathname).toBe("/login"));
  });

  it("sends a visitor to /setup while no admin exists", async () => {
    needsSetup = true;
    const router = renderAt("/welcome");
    await waitFor(() => expect(router.state.location.pathname).toBe("/setup"));
  });

  it("sends a user without environments.create to /, and never asks for the socket", async () => {
    me = VIEWER;
    const router = renderAt("/welcome");
    await waitFor(() => expect(router.state.location.pathname).toBe("/environments"));
    expect(calls.some((c) => c.path === "/api/local-host")).toBe(false);
  });

  it("sends the admin to / once the host is already managed", async () => {
    localHost = { ...DETECTED, managed: true };
    const router = renderAt("/welcome");
    await waitFor(() => expect(router.state.location.pathname).toBe("/"));
  });
});

describe("the signed-in home with no environments", () => {
  it("offers the opt-in to an admin and adds the host only on the click", async () => {
    renderAt("/");

    expect(await screen.findByText("No environments yet")).toBeTruthy();
    expect(await screen.findByRole("heading", { name: "Manage this Docker host?" })).toBeTruthy();
    await settled(queryClient);
    expect(posts()).toEqual([]);

    fireEvent.click(screen.getByRole("button", { name: "Manage this host" }));

    // The environment now exists, so the empty state goes away.
    await waitFor(() => expect(screen.queryByText("No environments yet")).toBeNull());
    expect(posts()).toHaveLength(1);
  });

  it("shows the docker run snippet and Check again when there is no socket", async () => {
    localHost = { socketPath: "/var/run/docker.sock", detected: false, managed: false };
    renderAt("/");

    expect(await screen.findByRole("heading", { name: "No Docker socket found" })).toBeTruthy();
    expect(await screen.findByRole("button", { name: "Copy the docker run command" })).toBeTruthy();
    // The overview's own empty state says what remote hosts need; the
    // after-setup footnote belongs to the wizard only.
    expect(screen.queryByText(/Remote hosts will be available/)).toBeNull();
    expect(screen.getByRole("button", { name: "Check again" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Continue" })).toBeNull();
  });

  it("tells a user who cannot add hosts to ask an admin, without asking for the socket", async () => {
    me = VIEWER;
    renderAt("/");

    expect(await screen.findByText("No environments yet")).toBeTruthy();
    expect(screen.getByText("Ask an admin to add an environment.")).toBeTruthy();
    expect(calls.some((c) => c.path === "/api/local-host")).toBe(false);
  });

  it("shows no empty state once an environment exists", async () => {
    environments = [{ ...ADDED }];
    renderAt("/");
    await waitFor(() => expect(calls.some((c) => c.path === "/api/environments")).toBe(true));
    await settled(queryClient);
    expect(screen.queryByText("No environments yet")).toBeNull();
    expect(screen.queryByRole("heading", { name: "Manage this Docker host?" })).toBeNull();
  });
});
