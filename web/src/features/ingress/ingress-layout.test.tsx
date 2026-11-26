// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { queryClient } from "@/api/query-client";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { routeTree } from "@/routeTree.gen";
import "@/i18n";
import { makeMe } from "@/test-me";
import { splitPermissions } from "@/test-permissions";

// The Ingress page's frame and its gate (UI-SPEC S1): the title, the one-line description and the tab bar
// for a reader, and the no-permission page for anyone else.

const ENV = "e1";
const PAGE = `/env/${ENV}/ingress`;
const BASE = `/api/env/${ENV}`;

let me: ReturnType<typeof makeMe>;
let routes: Record<string, unknown>[];
let status: Record<string, unknown>;
const defaults = queryClient.getDefaultOptions();

beforeEach(() => {
  me = makeMe({ envs: { [ENV]: "viewer" } });
  routes = [];
  status = { objectsKnown: true, paused: false, lastEnsure: { at: "", class: "" } };
  queryClient.setDefaultOptions({ queries: { retry: false, refetchOnWindowFocus: false } });
  setCsrfToken(null);
  FakeEventSource.reset();
  vi.stubGlobal("EventSource", FakeEventSource);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request): Promise<Response> => {
      const url = new URL(req.url);
      switch (url.pathname) {
        case "/api/instance":
          return Response.json({ instanceName: "Lab", needsSetup: false, rememberDays: 30 });
        case "/api/me":
          return Response.json(me);
        case "/api/version":
          return Response.json({
            commit: "",
            display: "v0.1.0",
            sourceTreeUrl: "https://example.test/tree",
            sourceUrl: "https://example.test",
            version: "v0.1.0",
          });
        case "/api/environments":
          return Response.json({
            items: [
              {
                id: ENV,
                name: "Lab swarm",
                kind: "local",
                type: "swarm-manager",
                envType: "swarm",
                enabled: true,
                isSelfHost: true,
                createdAt: "2026-10-01T10:00:00Z",
                status: { state: "online" },
              },
            ],
            total: 1,
          });
        case `${BASE}/summary`:
          return Response.json({
            environment: {
              id: ENV,
              name: "Lab swarm",
              type: "swarm-manager",
              status: { state: "online" },
            },
            engine: { name: "Docker Engine", version: "29.1.0", apiVersion: "1.52" },
            counts: {
              containers: 1,
              containersRunning: 1,
              containersStopped: 0,
              images: 0,
              volumes: 0,
              networks: 0,
            },
            isWorker: false,
            fetchedAt: "2026-10-01T10:00:00Z",
          });
        case `${BASE}/ingress/status`:
          return Response.json(status);
        case `${BASE}/ingress/routes`:
          return Response.json({ enabled: true, routes });
        case `${BASE}/ingress/certificates`:
          return Response.json({ items: [] });
        case `${BASE}/ingress/certificates/budget`:
          return Response.json({ items: [] });
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  queryClient.clear();
  queryClient.setDefaultOptions(defaults);
  document.title = "";
});

function renderAt(path = PAGE) {
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

describe("the Ingress page frame", () => {
  it("shows the title, the description and the Routes tab, active, for a reader", async () => {
    renderAt();
    expect(await screen.findByRole("heading", { level: 2, name: "Ingress" })).toBeTruthy();
    expect(
      screen.getByText(
        "Hostnames this environment serves, their certificates, and the nodes that serve them.",
      ),
    ).toBeTruthy();
    const tabs = screen.getByRole("tablist", { name: "Ingress sections" });
    expect(tabs).toBeTruthy();
    const routes = await screen.findByRole("tab", { name: "Routes" });
    expect(routes.getAttribute("href")).toBe(PAGE);
    await waitFor(() => expect(routes.getAttribute("data-active")).toBeTruthy());
    await waitFor(() => expect(document.title).toContain("Ingress"));
  });

  it("lists Certificates second, Nodes third and Domains fourth in the tab bar and marks Certificates active on its own route", async () => {
    renderAt(`${PAGE}/certificates`);
    const certificates = await screen.findByRole("tab", { name: "Certificates" });
    expect(screen.getAllByRole("tab").map((tab) => tab.textContent)).toEqual([
      "Routes",
      "Certificates",
      "Nodes",
      "Domains",
    ]);
    expect(certificates.getAttribute("href")).toBe(`${PAGE}/certificates`);
    expect(screen.getByRole("tab", { name: "Nodes" }).getAttribute("href")).toBe(`${PAGE}/nodes`);
    expect(screen.getByRole("tab", { name: "Domains" }).getAttribute("href")).toBe(
      `${PAGE}/domains`,
    );
    await waitFor(() => expect(certificates.getAttribute("data-active")).toBeTruthy());
    expect(screen.getByRole("tab", { name: "Routes" }).getAttribute("data-active")).toBeNull();
  });

  it("opens on the Routes tab, and its settling dot pulses only for a user who accepts motion", async () => {
    routes = [
      {
        kind: "route",
        hostname: "app.example.com",
        path: "/",
        ownerKind: "stack",
        port: 8080,
        reason: "",
        scheme: "http",
        service: "web",
        stackId: "s1",
        stackName: "shop",
        state: "pending",
        stopped: false,
        strip: false,
        conditions: [],
      },
    ];
    renderAt();
    const table = await screen.findByRole("table", { name: "Routes" });
    const dot = table.querySelector("span[aria-hidden='true'].animate-pulse");
    expect(dot).not.toBeNull();
    expect(dot?.className).toContain("motion-reduce:animate-none");
  });

  it("gives a user without ingress.read the no-permission page, with no frame", async () => {
    me = makeMe({
      envs: { [ENV]: "viewer" },
      permissions: splitPermissions(["containers.read", "environments.read"]),
    });
    renderAt();
    expect(await screen.findByText("You don't have permission to view this page")).toBeTruthy();
    expect(screen.queryByRole("heading", { level: 2, name: "Ingress" })).toBeNull();
    expect(screen.queryByRole("tab", { name: "Routes" })).toBeNull();
  });

  it("gives a user with no role on the environment the no-permission page", async () => {
    me = makeMe({ envs: {} });
    renderAt();
    expect(await screen.findByText("You don't have permission to view this page")).toBeTruthy();
    expect(screen.queryByRole("tab", { name: "Routes" })).toBeNull();
  });

  it("draws the paused alert once, between the tab bar and the tab, and not inside the tab", async () => {
    status = { objectsKnown: true, paused: true, lastEnsure: { at: "", class: "" } };
    routes = [
      {
        kind: "route",
        hostname: "app.example.com",
        path: "/",
        ownerKind: "stack",
        port: 8080,
        reason: "",
        scheme: "http",
        service: "web",
        stackId: "s1",
        stackName: "shop",
        state: "serving",
        stopped: false,
        strip: false,
        conditions: [],
      },
    ];
    renderAt();
    const text = await screen.findByText(/^Ingress is paused after a restore/);
    const table = await screen.findByRole("table", { name: "Routes" });
    expect(screen.getAllByText(/^Ingress is paused after a restore/)).toHaveLength(1);
    const tabs = screen.getByRole("tablist", { name: "Ingress sections" });
    expect(tabs.compareDocumentPosition(text) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(text.compareDocumentPosition(table) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(table.contains(text)).toBe(false);
  });
});
