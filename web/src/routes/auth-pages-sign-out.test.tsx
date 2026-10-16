// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

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
  permissions: { global: ["environments.create"], environments: {} },
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

interface Call {
  path: string;
  method: string;
  csrf: string | null;
}

let calls: Call[];
let signedIn: boolean;

beforeEach(() => {
  calls = [];
  signedIn = true;
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      calls.push({ path, method: req.method, csrf: req.headers.get("X-CSRF-Token") });
      if (path === "/api/instance") {
        return Response.json({ instanceName: "Lab", needsSetup: false, rememberDays: 30 });
      }
      if (path === "/api/me") {
        return signedIn ? Response.json(ADMIN) : Response.json({ status: 401 }, { status: 401 });
      }
      if (path === "/api/auth/logout") {
        signedIn = false;
        return new Response(null, { status: 204 });
      }
      if (path === "/api/local-host") return Response.json(DETECTED);
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

const sourceLinks = () =>
  screen.queryAllByRole("link", { name: "Source code (opens in a new tab)" });

describe("Log out outside the signed-in shell (W-3, AUTH-03)", () => {
  it("offers Log out on /welcome and signs out with the CSRF token", async () => {
    const router = renderAt("/welcome");
    expect(await screen.findByText("Signed in as admin")).toBeTruthy();
    await waitFor(() => expect(sourceLinks()).toHaveLength(1));
    fireEvent.click(screen.getByRole("button", { name: "Log out" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/login"));
    const logout = calls.find((c) => c.path === "/api/auth/logout");
    expect(logout?.method).toBe("POST");
    expect(logout?.csrf).toBe("csrf-1");
    expect(router.state.location.search).not.toHaveProperty("expired");
  });

  it("offers Log out on the not-found page to a signed-in user", async () => {
    const router = renderAt("/no-such-page");
    expect(await screen.findByText("Page not found")).toBeTruthy();
    expect(await screen.findByText("Signed in as admin")).toBeTruthy();
    await waitFor(() => expect(sourceLinks()).toHaveLength(1));
    fireEvent.click(screen.getByRole("button", { name: "Log out" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/login"));
    expect(calls.some((c) => c.path === "/api/auth/logout")).toBe(true);
  });

  it("shows neither the line nor the button to an anonymous visitor, and does not redirect", async () => {
    signedIn = false;
    const router = renderAt("/no-such-page");
    expect(await screen.findByText("Page not found")).toBeTruthy();
    await waitFor(() => expect(calls.some((c) => c.path === "/api/me")).toBe(true));
    await settled(queryClient);
    expect(screen.queryByText(/Signed in as/)).toBeNull();
    expect(screen.queryByRole("button", { name: "Log out" })).toBeNull();
    expect(router.state.location.pathname).toBe("/no-such-page");
    await waitFor(() => expect(sourceLinks()).toHaveLength(1));
  });

  it("adds no signed-in bar to /login and no /api/me request of its own", async () => {
    signedIn = false;
    renderAt("/login");
    await screen.findByLabelText("Username");
    await settled(queryClient);
    expect(screen.queryByText(/Signed in as/)).toBeNull();
    expect(screen.queryByRole("button", { name: "Log out" })).toBeNull();
    // The sign-in guard's own check is the only one.
    expect(calls.filter((c) => c.path === "/api/me")).toHaveLength(1);
    await waitFor(() => expect(sourceLinks()).toHaveLength(1));
  });
});
