// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { AppSidebar } from "@/components/app-sidebar";
import { SidebarProvider, SidebarTrigger } from "@/components/ui/sidebar";
import { TooltipProvider } from "@/components/ui/tooltip";
import "@/i18n";
import { makeMe } from "@/test-me";

// The switcher and the version footer have their own tests and their own endpoints.
vi.mock("@/components/env-switcher", () => ({ EnvSwitcher: () => null }));
vi.mock("@/components/version-footer", () => ({ VersionFooter: () => null }));

const DESKTOP_WIDTH = window.innerWidth;

beforeEach(() => {
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      if (url.pathname === "/api/me") return Response.json(makeMe({ envs: { e1: "viewer" } }));
      return Response.json({ items: [] });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  window.innerWidth = DESKTOP_WIDTH;
});

// A phone is what useIsMobile reads: a window narrower than 768px.
function mount(width: number) {
  window.innerWidth = width;
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const root = createRootRoute({
    component: () => (
      <TooltipProvider>
        <SidebarProvider>
          <SidebarTrigger aria-label="Toggle sidebar" />
          <AppSidebar />
        </SidebarProvider>
      </TooltipProvider>
    ),
  });
  const router = createRouter({
    routeTree: root.addChildren([
      createRoute({ getParentRoute: () => root, path: "/" }),
      createRoute({ getParentRoute: () => root, path: "/environments" }),
      createRoute({ getParentRoute: () => root, path: "/registries" }),
    ]),
    history: createMemoryHistory({ initialEntries: ["/"] }),
  });
  render(
    <QueryClientProvider client={client}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
  return router;
}

const openMenu = async () => {
  fireEvent.click(await screen.findByRole("button", { name: "Toggle sidebar" }));
  return screen.findByRole("dialog", { name: "Navigation" });
};

describe("the mobile main menu", () => {
  it("closes when a page is chosen, so the page is not left behind a drawer", async () => {
    const router = mount(375);
    const menu = await openMenu();
    fireEvent.click(await screen.findByRole("link", { name: "Registries" }));
    await waitFor(() => expect(screen.queryByRole("dialog", { name: "Navigation" })).toBeNull());
    await waitFor(() => expect(router.state.location.pathname).toBe("/registries"));
    expect(menu.isConnected).toBe(false);
  });

  it("closes again when the page you are already on is chosen", async () => {
    const router = mount(375);
    await router.navigate({ to: "/registries" });
    await openMenu();
    fireEvent.click(await screen.findByRole("link", { name: "Registries" }));
    await waitFor(() => expect(screen.queryByRole("dialog", { name: "Navigation" })).toBeNull());
  });

  it("has a Close menu button that closes it", async () => {
    mount(375);
    await openMenu();
    fireEvent.click(screen.getByRole("button", { name: "Close menu" }));
    await waitFor(() => expect(screen.queryByRole("dialog", { name: "Navigation" })).toBeNull());
    // And it opens again afterwards.
    await openMenu();
    expect(screen.getByRole("button", { name: "Close menu" })).toBeTruthy();
  });

  it("closes when the path changes some other way, such as the environment switcher or a redirect", async () => {
    const router = mount(375);
    await openMenu();
    await router.navigate({ to: "/environments" });
    await waitFor(() => expect(screen.queryByRole("dialog", { name: "Navigation" })).toBeNull());
  });
});

describe("the desktop sidebar", () => {
  it("has no Close menu button", async () => {
    mount(1280);
    expect(await screen.findByRole("link", { name: "Registries" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Close menu" })).toBeNull();
    expect(screen.queryByRole("dialog", { name: "Navigation" })).toBeNull();
  });
});
