// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { queryClient } from "@/api/query-client";
import { routeTree } from "@/routeTree.gen";
import "@/i18n";
import { makeMe } from "@/test-me";

const ME = makeMe({
  instanceAdmin: true,
  permissions: { global: ["environments.create"], environments: { "*": ["environments.read"] } },
});

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

const REGION = 'section[aria-label^="Notifications"]';

let signedIn: boolean;

beforeEach(() => {
  signedIn = true;
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/instance") {
        return Response.json({ instanceName: "Lab", needsSetup: false, rememberDays: 30 });
      }
      if (path === "/api/me") {
        return signedIn ? Response.json(ME) : Response.json({ status: 401 }, { status: 401 });
      }
      if (path === "/api/local-host") return Response.json(DETECTED);
      if (path === "/api/environments" && req.method === "POST") {
        return Response.json({ status: 500 }, { status: 500 });
      }
      if (path === "/api/environments") return Response.json({ items: [] });
      if (path === "/api/version") {
        return Response.json({
          commit: "",
          display: "v0.0.0",
          sourceTreeUrl: "https://example.test/tree",
          sourceUrl: "https://example.test",
          version: "v0.0.0",
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

describe("toasts outside the signed-in shell (WR-16)", () => {
  it("shows the failure toast on /welcome when Manage this host fails", async () => {
    const router = renderAt("/welcome");
    fireEvent.click(await screen.findByRole("button", { name: "Manage this host" }));
    expect(await screen.findByText("Something went wrong. Try again.")).toBeTruthy();
    expect(router.state.location.pathname).toBe("/welcome");
  });

  it("has exactly one toast region on /welcome", async () => {
    renderAt("/welcome");
    await screen.findByRole("heading", { name: "Manage this Docker host?" });
    expect(document.querySelectorAll(REGION)).toHaveLength(1);
  });

  it("has exactly one toast region on /login", async () => {
    signedIn = false;
    renderAt("/login");
    await screen.findByLabelText("Username");
    expect(document.querySelectorAll(REGION)).toHaveLength(1);
  });

  it("has exactly one toast region on the signed-in dashboard", async () => {
    renderAt("/");
    await screen.findByRole("button", { name: "User menu" });
    await waitFor(() => expect(document.querySelectorAll(REGION)).toHaveLength(1));
  });
});
