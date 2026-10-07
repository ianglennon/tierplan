// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { queryClient } from "@/api/query-client";
import { routeTree } from "@/routeTree.gen";
import "@/i18n";

const ADMIN = {
  user: { id: "u1", username: "admin", displayName: "", instanceAdmin: true },
  csrfToken: "csrf-1",
  permissions: { global: ["settings.read", "settings.write"], environments: {} },
  access: {},
  requirements: {},
  administrators: [],
};
const VIEWER = {
  user: { id: "u2", username: "viewer", displayName: "", instanceAdmin: false },
  csrfToken: "csrf-2",
  permissions: { global: [], environments: {} },
  access: {},
  requirements: {},
  administrators: [],
};

let me = ADMIN;
let warnings: Array<Record<string, string>> = [];
let warningRequests = 0;

beforeEach(() => {
  me = ADMIN;
  warnings = [];
  warningRequests = 0;
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/instance") {
        return Response.json({ instanceName: "Lab", needsSetup: false, rememberDays: 30 });
      }
      if (path === "/api/me") return Response.json(me);
      if (path === "/api/warnings") {
        warningRequests++;
        return Response.json({ items: warnings });
      }
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
  window.localStorage.clear();
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
}

describe("account and warnings in the app shell", () => {
  it("puts Change password between the role and the theme choice, and opens the dialog", async () => {
    renderAt("/");
    const trigger = await screen.findByRole("button", { name: "User menu" });
    fireEvent.keyDown(trigger, { key: "Enter" });
    const menu = await screen.findByRole("menu");
    const labels = within(menu)
      .getAllByRole("menuitem")
      .map((i) => i.textContent);
    expect(labels).toEqual(["Change password", "Log out"]);
    const text = menu.textContent ?? "";
    expect(text.indexOf("Instance admin")).toBeGreaterThanOrEqual(0);
    expect(text.indexOf("Instance admin")).toBeLessThan(text.indexOf("Change password"));
    expect(text.indexOf("Change password")).toBeLessThan(text.indexOf("Theme"));

    fireEvent.click(screen.getByRole("menuitem", { name: "Change password" }));
    const dialog = await screen.findByRole("dialog", { name: "Change password" });
    expect(within(dialog).getByLabelText("Current password")).toBeTruthy();

    // Closing returns focus to the menu trigger, not to the page.
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    await waitFor(() => expect(document.activeElement).toBe(trigger));
  });

  it("shows an admin the bell and, for a data directory that is lost with the container, the banner above the header", async () => {
    warnings = [
      { id: "data_not_persisted", reason: "not a mount point" },
      { id: "key_backup", keyPath: "/data/bayplan.key" },
    ];
    renderAt("/");
    expect(await screen.findByRole("button", { name: "Warnings, 1 item" })).toBeTruthy();
    const banner = await screen.findByText(/^Data isn't being persisted\./);
    const header = document.querySelector("header");
    expect(header).toBeTruthy();
    // The banner comes before the header in the document, so it sits above it.
    expect(
      banner.compareDocumentPosition(header as HTMLElement) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
  });

  it("shows a user without settings.read neither the bell nor the banner, and asks for no warnings", async () => {
    me = VIEWER;
    warnings = [{ id: "data_not_persisted", reason: "tmpfs" }];
    renderAt("/");
    await screen.findByRole("button", { name: "User menu" });
    expect(screen.queryByRole("button", { name: /^Warnings/ })).toBeNull();
    expect(screen.queryByText(/^Data isn't being persisted\./)).toBeNull();
    expect(warningRequests).toBe(0);
  });

  it("shows the user menu of anyone but an instance admin no role line", async () => {
    me = VIEWER;
    renderAt("/");
    const trigger = await screen.findByRole("button", { name: "User menu" });
    fireEvent.keyDown(trigger, { key: "Enter" });
    const menu = await screen.findByRole("menu");
    expect(menu.textContent).toContain("viewer");
    expect(menu.textContent).not.toContain("Instance admin");
  });

  it("mounts one toast region for the signed-in pages", async () => {
    renderAt("/");
    await screen.findByRole("button", { name: "User menu" });
    expect(document.querySelectorAll('section[aria-label^="Notifications"]')).toHaveLength(1);
  });
});
