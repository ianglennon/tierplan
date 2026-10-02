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

const ME = {
  user: { id: "u1", username: "admin", role: "admin" },
  csrfToken: "csrf-1",
  permissions: { global: [], environments: {} },
};

const UNREACHABLE = "Can't reach Bayplan. Check your connection and try again.";
const SOURCE_LINK = { name: "Source code (opens in a new tab)" };

let instanceStatus = 500;

beforeEach(() => {
  instanceStatus = 500;
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/instance") {
        return instanceStatus === 200
          ? Response.json({ instanceName: "Lab", needsSetup: false, rememberDays: 30 })
          : Response.json({ status: instanceStatus }, { status: instanceStatus });
      }
      if (path === "/api/me") return Response.json(ME);
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

describe("route error page (WR-18)", () => {
  it("shows the translated page with Retry and one footer when the signed-in guard fails", async () => {
    renderAt("/");
    expect(await screen.findByText(UNREACHABLE)).toBeTruthy();
    expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
    await waitFor(() => expect(screen.queryAllByRole("link", SOURCE_LINK)).toHaveLength(1));
    // Never TanStack's default error screen.
    expect(screen.queryByText(/GET \/api\/instance failed/)).toBeNull();
    expect(screen.queryByText(/Something went wrong!/)).toBeNull();
  });

  it("Retry re-runs the route and renders the page once the API answers", async () => {
    renderAt("/");
    fireEvent.click(await screen.findByRole("button", { name: "Retry" }));
    instanceStatus = 200;
    // The first click may race the stub; click again until the page renders.
    await waitFor(async () => {
      const retry = screen.queryByRole("button", { name: "Retry" });
      if (retry) fireEvent.click(retry);
      expect(screen.getByText("No environments yet")).toBeTruthy();
    });
  });

  it("shows the same page, with one footer, when the sign-in guard fails", async () => {
    renderAt("/login");
    expect(await screen.findByText(UNREACHABLE)).toBeTruthy();
    expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
    await waitFor(() => expect(screen.queryAllByRole("link", SOURCE_LINK)).toHaveLength(1));
  });
});
