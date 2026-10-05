// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { api, setCsrfToken } from "@/api/client";
import { queryClient } from "@/api/query-client";
import { routeTree } from "@/routeTree.gen";
import "@/i18n";

const ME = {
  user: { id: "u1", username: "admin", role: "admin" },
  csrfToken: "csrf-1",
  permissions: { global: [], environments: { "*": ["environments.read"] } },
};

interface Sent {
  path: string;
  method: string;
  csrf: string | null;
}

let sent: Sent[] = [];
let meStatus = 200;
let logoutAnswer: () => Response;

beforeEach(() => {
  sent = [];
  meStatus = 200;
  setCsrfToken(null);
  logoutAnswer = () => {
    meStatus = 401;
    return new Response(null, { status: 204 });
  };
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      sent.push({ path, method: req.method, csrf: req.headers.get("X-CSRF-Token") });
      if (path === "/api/instance") {
        return Response.json({ instanceName: "Lab", needsSetup: false, rememberDays: 30 });
      }
      if (path === "/api/me") {
        return meStatus === 200
          ? Response.json(ME)
          : Response.json({ status: meStatus }, { status: meStatus });
      }
      if (path === "/api/auth/logout") return logoutAnswer();
      if (path === "/api/environments" && req.method === "POST") {
        return Response.json({}, { status: 400 });
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

async function clickLogOut() {
  const trigger = await screen.findByRole("button", { name: "User menu" });
  fireEvent.keyDown(trigger, { key: "Enter" });
  fireEvent.click(await screen.findByRole("menuitem", { name: "Log out" }));
}

const logoutCalls = () => sent.filter((s) => s.path === "/api/auth/logout").length;

describe("sign out from the user menu (WR-17)", () => {
  for (const status of [204, 401]) {
    it(`leaves the signed-in state when the server answers ${status}`, async () => {
      logoutAnswer = () => {
        meStatus = 401;
        return status === 204
          ? new Response(null, { status })
          : Response.json({ status }, { status });
      };
      const router = renderAt("/");
      await screen.findByText("No environments yet");
      await clickLogOut();
      await waitFor(() => expect(router.state.location.pathname).toBe("/login"));
      expect(router.state.location.search).not.toHaveProperty("expired");
      await waitFor(() => expect(queryClient.getQueryData(["me"])).toBeUndefined());
      // The token is gone: a later mutating request carries none.
      sent = [];
      await api.POST("/api/environments", { body: { name: "x", kind: "local" } as never });
      expect(sent.find((s) => s.path === "/api/environments")?.csrf).toBeNull();
    });
  }

  it("stays put, says so and keeps the session usable when the server refuses (403)", async () => {
    logoutAnswer = () => Response.json({ status: 403 }, { status: 403 });
    const router = renderAt("/");
    await screen.findByText("No environments yet");
    await clickLogOut();
    expect(await screen.findByText("Something went wrong. Try again.")).toBeTruthy();
    expect(logoutCalls()).toBe(1);
    expect(router.state.location.pathname).toBe("/environments");
    // The CSRF token and the cache are untouched.
    expect(queryClient.getQueryData(["me"])).toBeTruthy();
    sent = [];
    await api.POST("/api/environments", { body: { name: "x", kind: "local" } as never });
    expect(sent.find((s) => s.path === "/api/environments")?.csrf).toBe("csrf-1");
    // The expired-session handler is armed again: a later 401 still signs out.
    meStatus = 401;
    await api.GET("/api/me");
    await waitFor(() => expect(router.state.location.pathname).toBe("/login"));
    expect(router.state.location.search).toMatchObject({ expired: true });
  });

  it("stays put and says the server is unreachable when the request fails", async () => {
    logoutAnswer = () => {
      throw new TypeError("network down");
    };
    const router = renderAt("/");
    await screen.findByText("No environments yet");
    await clickLogOut();
    expect(
      await screen.findByText("Can't reach Bayplan. Check your connection and try again."),
    ).toBeTruthy();
    expect(router.state.location.pathname).toBe("/environments");
    expect(queryClient.getQueryData(["me"])).toBeTruthy();
  });

  it("treats a 429 and a 500 as a refusal too", async () => {
    for (const status of [429, 500]) {
      logoutAnswer = () => Response.json({ status }, { status });
      const router = renderAt("/");
      await screen.findByText("No environments yet");
      await clickLogOut();
      expect(
        (await screen.findAllByText("Something went wrong. Try again.")).length,
      ).toBeGreaterThan(0);
      expect(router.state.location.pathname).toBe("/environments");
      cleanup();
    }
  });
});
