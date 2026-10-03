// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { queryClient } from "@/api/query-client";
import { retryTiming } from "@/routes/_app/env/$envId";
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
  type: "swarm-manager",
  enabled: true,
  isSelfHost: true,
  createdAt: "2026-10-01T10:00:00Z",
  engine: { name: "Docker Engine", version: "29.8.2", apiVersion: "1.54" },
  status: { state: "online" },
};

const SUMMARY = {
  environment: { id: "e1", name: "Lab host", type: "swarm-manager", status: { state: "online" } },
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
  fetchedAt: "2026-10-01T10:00:00Z",
};

function problem(status: number, type: string, extra: Record<string, unknown> = {}) {
  return Response.json({ type, title: "x", status, ...extra }, { status });
}

interface Stub {
  environments: () => Response;
  summary: () => Response;
  reconnect: () => Response | Promise<Response>;
}

let sent: { path: string; method: string; csrf: string | null }[] = [];
let stub: Stub;

function stubApi() {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      sent.push({ path, method: req.method, csrf: req.headers.get("X-CSRF-Token") });
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
      if (path === "/api/environments") return stub.environments();
      if (path === "/api/env/e1/summary") return stub.summary();
      if (path === "/api/env/e1/reconnect") return stub.reconnect();
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

const countSummaryReads = () => sent.filter((s) => s.path === "/api/env/e1/summary").length;

beforeEach(() => {
  sent = [];
  setCsrfToken(null);
  retryTiming.delayMs = 1;
  retryTiming.polls = 2;
  stub = {
    environments: () => Response.json({ items: [LAB] }),
    summary: () => Response.json(SUMMARY),
    reconnect: () => Response.json(LAB, { status: 202 }),
  };
  stubApi();
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  queryClient.clear();
  window.localStorage.clear();
  document.title = "";
});

describe("environment layout full-page states", () => {
  it("shows Docker isn't reachable, naming the environment and the time, with Retry (D-25)", async () => {
    stub.summary = () =>
      problem(503, "environment_offline", { since: "2026-10-01T14:05:00Z", reason: "unreachable" });
    renderAt("/env/e1");
    expect(await screen.findByRole("heading", { name: "Docker isn't reachable" })).toBeTruthy();
    const body = screen.getByText(/Bayplan lost its connection to Lab host at \d{1,2}:\d{2}/);
    expect(body.textContent).toContain("/var/run/docker.sock is mounted");
    expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
    expect(document.title).toBe("Docker isn't reachable · Lab");
  });

  it("leaves the time out when the engine never answered", async () => {
    stub.summary = () => problem(503, "environment_offline", { reason: "unreachable" });
    renderAt("/env/e1");
    const body = await screen.findByText(/Bayplan lost its connection to Lab host\./);
    expect(body.textContent).not.toMatch(/ at \d/);
  });

  it("keeps the rest of the UI working while the environment is down (D-26)", async () => {
    stub.summary = () => problem(503, "environment_offline", { since: "2026-10-01T14:05:00Z" });
    renderAt("/env/e1");
    await screen.findByRole("heading", { name: "Docker isn't reachable" });
    expect(screen.getByRole("button", { name: "User menu" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Switch environment" })).toBeTruthy();
    expect(screen.getAllByRole("link", { name: "Source code (opens in a new tab)" })).toHaveLength(
      1,
    );
  });

  it("shows This engine isn't supported with the engine, version and API (D-58)", async () => {
    stub.summary = () =>
      problem(409, "environment_unsupported", {
        reason: "api_too_old",
        engine: { name: "Docker Engine", version: "23.0.1", apiVersion: "1.42" },
      });
    renderAt("/env/e1");
    expect(
      await screen.findByRole("heading", { name: "This engine isn't supported" }),
    ).toBeTruthy();
    expect(
      screen.getByText(
        "Docker Engine 23.0.1 (API 1.42) is below the minimum Bayplan supports, Docker Engine 24. Upgrade Docker Engine, then retry.",
      ),
    ).toBeTruthy();
    expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
  });

  it("uses the Podman copy for Podman", async () => {
    stub.summary = () =>
      problem(409, "environment_unsupported", {
        reason: "podman",
        engine: { name: "Podman", version: "5.2.0", apiVersion: "1.41" },
      });
    renderAt("/env/e1");
    expect(await screen.findByRole("heading", { name: "Podman isn't supported" })).toBeTruthy();
    expect(screen.getByText("Bayplan manages Docker Engine only.")).toBeTruthy();
  });

  it("renders the not-found copy for an unknown environment and the permission page on 403", async () => {
    stub.summary = () => problem(404, "environment_not_found");
    renderAt("/env/e1");
    expect(await screen.findByRole("heading", { name: "Page not found" })).toBeTruthy();
    expect(screen.getByRole("link", { name: "Go to the dashboard" })).toBeTruthy();
    cleanup();
    queryClient.clear();

    stub.summary = () => problem(403, "about:blank");
    renderAt("/env/e1");
    expect(
      await screen.findByRole("heading", { name: "You don't have permission to view this page" }),
    ).toBeTruthy();
    expect(screen.getByText("Ask an admin for access.")).toBeTruthy();
  });

  it("does not take a plain 503 from a proxy for an offline environment", async () => {
    stub.summary = () => new Response("bad gateway", { status: 503 });
    renderAt("/env/e1");
    await waitFor(() => expect(countSummaryReads()).toBeGreaterThan(0));
    await screen.findByRole("button", { name: "Switch environment" });
    expect(screen.queryByRole("heading", { name: "Docker isn't reachable" })).toBeNull();
  });
});

describe("Retry", () => {
  it("posts the reconnect, shows Retrying…, and replaces the state when Docker answers", async () => {
    stub.summary = () => problem(503, "environment_offline", { since: "2026-10-01T14:05:00Z" });
    renderAt("/env/e1");
    const button = await screen.findByRole("button", { name: "Retry" });
    // Docker answers from now on, but the reconnect request is held open so the
    // pending state can be seen.
    stub.summary = () => Response.json(SUMMARY);
    let release: () => void = () => {};
    stub.reconnect = () =>
      new Promise<Response>((resolve) => {
        release = () => resolve(Response.json(LAB, { status: 202 }));
      });
    fireEvent.click(button);
    expect(await screen.findByRole("button", { name: "Retrying…" })).toHaveProperty(
      "disabled",
      true,
    );
    release();
    await waitFor(() =>
      expect(screen.queryByRole("heading", { name: "Docker isn't reachable" })).toBeNull(),
    );
    const reconnect = sent.filter((s) => s.path === "/api/env/e1/reconnect");
    expect(reconnect).toHaveLength(1);
    expect(reconnect[0]?.method).toBe("POST");
    expect(reconnect[0]?.csrf).toBe("csrf-1");
  });

  it("re-renders the state with Attempted when it is still down", async () => {
    stub.summary = () => problem(503, "environment_offline", { since: "2026-10-01T14:05:00Z" });
    renderAt("/env/e1");
    fireEvent.click(await screen.findByRole("button", { name: "Retry" }));
    expect(await screen.findByText("Attempted just now")).toBeTruthy();
    expect(screen.getByRole("heading", { name: "Docker isn't reachable" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Retry" })).toHaveProperty("disabled", false);
  });
});

describe("environment switcher", () => {
  it("shows the name, type badge and a health dot with text in the trigger and menu (D-54)", async () => {
    renderAt("/env/e1");
    const trigger = await screen.findByRole("button", { name: "Switch environment" });
    await waitFor(() => expect(within(trigger).getByText("Lab host")).toBeTruthy());
    expect(within(trigger).getByText("Swarm")).toBeTruthy();
    expect(within(trigger).getByRole("img", { name: "Online" })).toBeTruthy();

    fireEvent.keyDown(trigger, { key: "Enter" });
    const menu = await screen.findByRole("menu");
    expect(within(menu).getByText("Lab host")).toBeTruthy();
    expect(within(menu).getByText("Swarm")).toBeTruthy();
    expect(within(menu).getByText("Online")).toBeTruthy();
    expect(within(menu).getByRole("menuitem", { name: /Manage environments/ })).toBeTruthy();
  });

  it("pairs an offline environment's dot with its text", async () => {
    stub.environments = () =>
      Response.json({
        items: [{ ...LAB, status: { state: "offline", since: "2026-10-01T14:05:00Z" } }],
      });
    renderAt("/env/e1");
    const trigger = await screen.findByRole("button", { name: "Switch environment" });
    await waitFor(() => expect(within(trigger).getByRole("img", { name: "Offline" })).toBeTruthy());
    fireEvent.keyDown(trigger, { key: "Enter" });
    const menu = await screen.findByRole("menu");
    expect(within(menu).getByText(/Offline since \d{1,2}:\d{2}/)).toBeTruthy();
  });

  it("shows No environment with Manage environments when there are none", async () => {
    stub.environments = () => Response.json({ items: [] });
    renderAt("/");
    const trigger = await screen.findByRole("button", { name: "Switch environment" });
    expect(within(trigger).getByText("No environment")).toBeTruthy();
    fireEvent.keyDown(trigger, { key: "Enter" });
    expect(await screen.findByRole("menuitem", { name: /Manage environments/ })).toBeTruthy();
  });

  it("keeps the last known name and adds Couldn't load environments when the list fails", async () => {
    renderAt("/env/e1");
    const trigger = await screen.findByRole("button", { name: "Switch environment" });
    await waitFor(() => expect(within(trigger).getByText("Lab host")).toBeTruthy());

    stub.environments = () => new Response("boom", { status: 500 });
    void queryClient.invalidateQueries({ queryKey: ["environments"] });
    fireEvent.keyDown(trigger, { key: "Enter" });
    const item = await screen.findByText("Couldn't load environments", {}, { timeout: 4000 });
    expect(item.closest("[role=menuitem]")?.getAttribute("aria-disabled")).toBe("true");
    expect(within(trigger).getByText("Lab host")).toBeTruthy();
  }, 8000);

  it("truncates a long name with a title and keeps the badge and dot", async () => {
    const long = "a-very-long-environment-name-".repeat(4);
    stub.environments = () => Response.json({ items: [{ ...LAB, name: long }] });
    renderAt("/env/e1");
    const trigger = await screen.findByRole("button", { name: "Switch environment" });
    const name = await within(trigger).findByText(long);
    expect(name.className).toContain("truncate");
    expect(within(trigger).getByText("Swarm").className).toContain("shrink-0");
    expect(within(trigger).getByRole("img", { name: "Online" }).className).toContain("shrink-0");
  });
});
