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
import { makeMe } from "@/test-me";

function userBody(over: Record<string, unknown> = {}) {
  return {
    id: "u1",
    username: "alice",
    displayName: "",
    email: "",
    instanceAdmin: false,
    status: "active",
    teams: [],
    teamCount: 0,
    createdAt: "2026-10-01T10:00:00Z",
    updatedAt: "2026-10-07T10:00:00.123456789Z",
    ...over,
  };
}

let current: Record<string, unknown>;
let user: Record<string, unknown>;
// What GET /api/users/u1 answers: the user, or a status for a failure.
let getUser: () => Response | Promise<Response>;
let lifecycle: (path: string) => Response | Promise<Response>;
let seen: string[];

beforeEach(() => {
  current = makeMe({ instanceAdmin: true });
  user = userBody();
  getUser = () => Response.json(user);
  lifecycle = (path) => {
    user = { ...user, status: path.endsWith("/disable") ? "disabled" : "active" };
    return Response.json(user);
  };
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
      if (path === "/api/users" && req.method === "GET") return Response.json([user]);
      if (path === "/api/users/u1" && req.method === "GET") return getUser();
      if (/^\/api\/users\/u1\/(disable|enable)$/.test(path)) return lifecycle(path);
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

const PAGE = "/settings/access/users/u1";

describe("the user page", () => {
  it("shows the breadcrumb, the username as the title and the status", async () => {
    renderAt(PAGE);
    const nav = await screen.findByRole("navigation", { name: "Breadcrumb" });
    expect(
      within(nav)
        .getAllByRole("listitem")
        .map((li) => li.textContent),
    ).toEqual(["Settings", "Access", "Users", "alice"]);
    expect(within(nav).getByRole("link", { name: "Users" }).getAttribute("href")).toBe(
      "/settings/access/users",
    );
    expect(screen.getByRole("heading", { level: 2, name: "alice" })).toBeTruthy();
    expect(screen.getByText("Active")).toBeTruthy();
    expect(document.title).toBe("alice · Lab");
  });

  it("titles the page with the display name and puts the username beneath it, next to the badges", async () => {
    user = userBody({ displayName: "Alice Liddell", instanceAdmin: true, status: "disabled" });
    renderAt(PAGE);
    expect(await screen.findByRole("heading", { level: 2, name: "Alice Liddell" })).toBeTruthy();
    // The username is beneath the title as well as in the breadcrumb.
    expect(screen.getAllByText("alice")).toHaveLength(2);
    expect(screen.getByText("Instance admin")).toBeTruthy();
    expect(screen.getByText("Disabled")).toBeTruthy();
  });

  it("shows a skeleton in the final layout while the user loads", async () => {
    getUser = () => new Promise<Response>(() => undefined);
    renderAt(PAGE);
    const busy = await screen.findByText("Loading user");
    expect((busy.parentElement as HTMLElement).getAttribute("aria-busy")).toBe("true");
  });

  it("shows the list error with Retry when the load fails, and loads on Retry", async () => {
    getUser = () => Response.json({ status: 500, title: "x", detail: "x" }, { status: 500 });
    renderAt(PAGE);
    // One automatic retry a second later comes first.
    expect(await screen.findByText("Couldn't load users.", {}, { timeout: 3000 })).toBeTruthy();
    getUser = () => Response.json(user);
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByRole("heading", { level: 2, name: "alice" })).toBeTruthy();
  });

  it("shows the Page not found page for an unknown or deleted id", async () => {
    getUser = () =>
      Response.json(
        { status: 404, type: "user_not_found", title: "Not Found", detail: "x" },
        { status: 404 },
      );
    renderAt(PAGE);
    expect(await screen.findByText("Page not found")).toBeTruthy();
    expect(seen.filter((s) => s === "GET /api/users/u1")).toHaveLength(1); // a 404 is not retried
  });

  it("is refused for someone who does not hold users.manage", async () => {
    current = makeMe({ envs: { e1: "admin" } });
    renderAt(PAGE);
    await waitFor(() => expect(screen.queryByRole("heading", { name: "alice" })).toBeNull());
    expect(seen).not.toContain("GET /api/users/u1");
  });
});

describe("Disable user and Enable user", () => {
  it("asks first, with focus on Cancel, then disables, says so and shows the Disabled badge", async () => {
    renderAt(PAGE);
    fireEvent.click(await screen.findByRole("button", { name: "Disable user" }));
    const confirm = await screen.findByRole("alertdialog", { name: "Disable alice?" });
    expect(
      within(confirm).getByText(
        "alice is signed out everywhere and can't sign in until you enable them again. Their access and team memberships are kept.",
      ),
    ).toBeTruthy();
    expect(document.activeElement).toBe(within(confirm).getByRole("button", { name: "Cancel" }));
    expect(seen).not.toContain("POST /api/users/u1/disable");

    fireEvent.click(within(confirm).getByRole("button", { name: "Disable user" }));
    expect(await screen.findByText("alice is disabled.")).toBeTruthy();
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(seen.filter((s) => s === "POST /api/users/u1/disable")).toHaveLength(1);
    // The button now offers the opposite and the badge shows the new state.
    expect(await screen.findByRole("button", { name: "Enable user" })).toBeTruthy();
    expect(screen.getByText("Disabled")).toBeTruthy();
  });

  it("enables with one press, toasts and shows Active", async () => {
    user = userBody({ status: "disabled" });
    renderAt(PAGE);
    fireEvent.click(await screen.findByRole("button", { name: "Enable user" }));
    expect(await screen.findByText("alice is enabled.")).toBeTruthy();
    expect(await screen.findByRole("button", { name: "Disable user" })).toBeTruthy();
    expect(screen.getByText("Active")).toBeTruthy();
    expect(screen.queryByRole("alertdialog")).toBeNull();
  });

  it("keeps the dialog open with the reason when the server refuses the last admin", async () => {
    lifecycle = () =>
      Response.json(
        { status: 409, type: "last_instance_admin", title: "Conflict", detail: "x" },
        { status: 409 },
      );
    renderAt(PAGE);
    fireEvent.click(await screen.findByRole("button", { name: "Disable user" }));
    const confirm = await screen.findByRole("alertdialog");
    fireEvent.click(within(confirm).getByRole("button", { name: "Disable user" }));
    const alert = await within(confirm).findByRole("alert");
    expect(alert.textContent).toBe(
      "Bayplan needs at least one active instance admin. Make someone else an instance admin first.",
    );
    // The page reads the user again.
    await waitFor(() =>
      expect(seen.filter((s) => s === "GET /api/users/u1").length).toBeGreaterThan(1),
    );
  });
});
