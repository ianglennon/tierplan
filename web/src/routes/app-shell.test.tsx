// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
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
// What GET /api/environments/warnings answers; the bell of a reader lists it.
let envWarnings: unknown[] = [];

function stubApi() {
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
      if (path === "/api/auth/logout") {
        // The session ends server-side, so /api/me no longer knows the caller.
        meStatus = 401;
        return new Response(null, { status: 204 });
      }
      if (path === "/api/environments") return Response.json({ items: [] });
      if (path === "/api/environments/warnings") return Response.json({ items: envWarnings });
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

function setViewport(width: number) {
  Object.defineProperty(window, "innerWidth", { value: width, configurable: true });
}

const SOURCE_LINK = { name: "Source code (opens in a new tab)" };

// sourceLinks counts the source-code links on the page, which is how many
// version footers are visible (each footer has exactly one).
const sourceLinks = () => screen.queryAllByRole("link", SOURCE_LINK);

beforeEach(() => {
  sent = [];
  meStatus = 200;
  envWarnings = [];
  setCsrfToken(null);
  setViewport(1024);
  stubApi();
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  queryClient.clear();
  window.localStorage.clear();
  document.documentElement.classList.remove("dark");
  document.title = "";
});

async function openUserMenu() {
  const trigger = await screen.findByRole("button", { name: "User menu" });
  fireEvent.keyDown(trigger, { key: "Enter" });
  return trigger;
}

describe("app shell", () => {
  it("shows the no-environments landing with the UI-SPEC copy", async () => {
    renderAt("/");
    expect(await screen.findByText("No environments yet")).toBeTruthy();
    expect(
      screen.getByText(/Bayplan gives you one command that installs a small agent on it/),
    ).toBeTruthy();
    expect(document.title).toBe("Environments · Lab");
  });

  it("puts the skip link first and points it at the main landmark", async () => {
    renderAt("/");
    await screen.findByText("No environments yet");
    const links = screen.getAllByRole("link");
    expect(links[0]?.textContent).toBe("Skip to content");
    expect(links[0]?.getAttribute("href")).toBe("#main");
    expect(document.getElementById("main")?.tagName).toBe("MAIN");
  });

  it("gives the sidebar toggle an accessible name", async () => {
    renderAt("/");
    expect(await screen.findByRole("button", { name: "Toggle sidebar" })).toBeTruthy();
  });

  it("loads the CSRF token from /api/me on a fresh page, never from storage", async () => {
    renderAt("/");
    await screen.findByText("No environments yet");
    await api.POST("/api/auth/logout");
    expect(sent.filter((s) => s.path === "/api/auth/logout")[0]?.csrf).toBe("csrf-1");
    expect(JSON.stringify({ ...window.localStorage, ...window.sessionStorage })).not.toContain(
      "csrf-1",
    );
  });
});

describe("header bell", () => {
  it("is there for someone who may read an environment but not the settings, with their environments' problems", async () => {
    envWarnings = [
      { id: "env_offline", severity: "error", envId: "e1", envName: "Edge", reason: "timeout" },
    ];
    renderAt("/");
    expect(await screen.findByRole("button", { name: "Warnings, 1 item" })).toBeTruthy();
    // Instance warnings are an admin's: no settings.read, no request for them.
    expect(sent.some((s) => s.path === "/api/warnings")).toBe(false);
  });
});

describe("version footer (PLAT-05)", () => {
  it("sits in the sidebar, once, while the sidebar is expanded", async () => {
    renderAt("/");
    await screen.findByText("No environments yet");
    await waitFor(() => expect(sourceLinks()).toHaveLength(1));
    const sidebar = document.querySelector("[data-slot=sidebar]") as HTMLElement;
    expect(within(sidebar).getAllByRole("link", SOURCE_LINK)).toHaveLength(1);
  });

  it("moves to the content area, once, when the sidebar collapses to the rail", async () => {
    renderAt("/");
    await screen.findByText("No environments yet");
    fireEvent.click(screen.getByRole("button", { name: "Toggle sidebar" }));
    await waitFor(() => {
      const sidebar = document.querySelector("[data-slot=sidebar]") as HTMLElement;
      expect(within(sidebar).queryAllByRole("link", SOURCE_LINK)).toHaveLength(0);
    });
    expect(sourceLinks()).toHaveLength(1);
    expect(document.querySelector("main")?.parentElement?.contains(sourceLinks()[0] ?? null)).toBe(
      true,
    );
    expect(window.localStorage.getItem("bayplan-sidebar")).toBe("collapsed");
  });

  it("starts collapsed when the browser remembers that", async () => {
    window.localStorage.setItem("bayplan-sidebar", "collapsed");
    renderAt("/");
    await screen.findByText("No environments yet");
    await waitFor(() => expect(sourceLinks()).toHaveLength(1));
    const sidebar = document.querySelector("[data-slot=sidebar]") as HTMLElement;
    expect(within(sidebar).queryAllByRole("link", SOURCE_LINK)).toHaveLength(0);
  });

  it("is in the content area, once, while the mobile drawer is closed", async () => {
    setViewport(375);
    renderAt("/");
    await screen.findByText("No environments yet");
    await waitFor(() => expect(sourceLinks()).toHaveLength(1));
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("is in the drawer, once, while the mobile drawer is open, and back after it closes", async () => {
    setViewport(375);
    renderAt("/");
    await screen.findByText("No environments yet");
    fireEvent.click(screen.getByRole("button", { name: "Toggle sidebar" }));
    const drawer = await screen.findByRole("dialog");
    await waitFor(() => expect(within(drawer).getAllByRole("link", SOURCE_LINK)).toHaveLength(1));
    expect(sourceLinks()).toHaveLength(1);
    fireEvent.keyDown(drawer, { key: "Escape" });
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    await waitFor(() => expect(sourceLinks()).toHaveLength(1));
  });
});

describe("user menu", () => {
  it("shows the username, the role and the theme choice", async () => {
    renderAt("/");
    await openUserMenu();
    expect(await screen.findByText("admin")).toBeTruthy();
    expect(screen.getByText("Admin")).toBeTruthy();
    expect(screen.getByRole("menuitemradio", { name: "Light" })).toBeTruthy();
    expect(screen.getByRole("menuitemradio", { name: "Dark" })).toBeTruthy();
    expect(screen.getByRole("menuitemradio", { name: "System" })).toBeTruthy();
    expect(screen.getByRole("menuitem", { name: "Log out" })).toBeTruthy();
  });

  it("remembers the theme in bayplan-theme and applies it", async () => {
    renderAt("/");
    await openUserMenu();
    fireEvent.click(await screen.findByRole("menuitemradio", { name: "Dark" }));
    expect(window.localStorage.getItem("bayplan-theme")).toBe("dark");
    expect(document.documentElement.classList.contains("dark")).toBe(true);
  });

  it("logs out with the CSRF token, clears what it held and returns to /login", async () => {
    const router = renderAt("/");
    await openUserMenu();
    fireEvent.click(await screen.findByRole("menuitem", { name: "Log out" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/login"));
    const logout = sent.find((s) => s.path === "/api/auth/logout");
    expect(logout?.method).toBe("POST");
    expect(logout?.csrf).toBe("csrf-1");
    // The signed-in answer is gone, so the next sign-in is not shown stale data.
    await waitFor(() => expect(queryClient.getQueryData(["me"])).toBeUndefined());
    // A deliberate sign-out is not an expired session.
    expect(router.state.location.search).not.toHaveProperty("expired");
  });
});

describe("expired session", () => {
  it("sends the browser to /login with next and the notice after a later 401", async () => {
    const router = renderAt("/");
    await screen.findByText("No environments yet");
    meStatus = 401;
    // Whatever the ended session cached, wherever it lives (WR-15).
    queryClient.setQueryData(["environments"], { items: [{ id: "e1" }] });
    queryClient.setQueryData(["settings"], { general: {} });
    queryClient.setQueryData(["env", "e1", "containers"], { items: [] });
    window.history.pushState({}, "", "/somewhere?x=1");
    await api.GET("/api/me");
    await waitFor(() => expect(router.state.location.pathname).toBe("/login"));
    expect(router.state.location.search).toMatchObject({ next: "/somewhere?x=1", expired: true });
    expect(await screen.findByText("Your session expired. Sign in again.")).toBeTruthy();
    // Nothing from the ended session survives for the next person.
    await waitFor(() => expect(queryClient.getQueryData(["environments"])).toBeUndefined());
    expect(queryClient.getQueryData(["settings"])).toBeUndefined();
    expect(queryClient.getQueryData(["env", "e1", "containers"])).toBeUndefined();
    expect(queryClient.getQueryData(["me"])).toBeUndefined();
    window.history.pushState({}, "", "/");
  });
});

describe("instance answer reuse (WR-18)", () => {
  const instanceCalls = () => sent.filter((s) => s.path === "/api/instance").length;

  it("does not refetch /api/instance on navigation between signed-in pages within 30 s", async () => {
    const router = renderAt("/");
    await screen.findByText("No environments yet");
    const before = instanceCalls();
    expect(before).toBeGreaterThan(0);

    await router.navigate({ to: "/audit" });
    await waitFor(() => expect(router.state.location.pathname).toBe("/audit"));
    await router.navigate({ to: "/" });
    await screen.findByText("No environments yet");
    await new Promise((r) => setTimeout(r, 50));

    expect(instanceCalls()).toBe(before);
  });

  it("still fetches it fresh for the /login guard", async () => {
    meStatus = 401;
    renderAt("/login");
    await screen.findByLabelText("Username");
    const first = instanceCalls();
    cleanup();
    renderAt("/login");
    await screen.findByLabelText("Username");
    expect(instanceCalls()).toBeGreaterThan(first);
  });
});
