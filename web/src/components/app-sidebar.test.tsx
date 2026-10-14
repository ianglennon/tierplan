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
import { splitPermissions } from "@/test-permissions";

// The switcher and the version footer have their own tests and their own endpoints.
vi.mock("@/components/env-switcher", () => ({ EnvSwitcher: () => null }));
vi.mock("@/components/version-footer", () => ({ VersionFooter: () => null }));

const DESKTOP_WIDTH = window.innerWidth;

// What the stubbed server says: who is signed in and what kind of environment e1 is.
let me: ReturnType<typeof makeMe>;
let envType: "swarm" | "standalone";

beforeEach(() => {
  setCsrfToken(null);
  me = makeMe({ envs: { e1: "viewer" } });
  envType = "swarm";
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      if (url.pathname === "/api/me") return Response.json(me);
      if (url.pathname === "/api/environments") {
        return Response.json({ items: [{ id: "e1", name: "Env one", envType }] });
      }
      // The summary is not what these tests are about: the environment list names the type.
      if (url.pathname.endsWith("/summary")) return Response.json({}, { status: 404 });
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
function mount(width: number, path = "/") {
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
      createRoute({ getParentRoute: () => root, path: "/env/$envId" }),
      createRoute({ getParentRoute: () => root, path: "/env/$envId/containers" }),
      createRoute({ getParentRoute: () => root, path: "/env/$envId/services" }),
      createRoute({ getParentRoute: () => root, path: "/env/$envId/placement" }),
    ]),
    history: createMemoryHistory({ initialEntries: [path] }),
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

describe("the Swarm group", () => {
  it("lists Services below Resources on a swarm environment, for a user who can read services", async () => {
    mount(1280, "/env/e1");
    const link = await screen.findByRole("link", { name: "Services" });
    expect(link.getAttribute("href")).toBe("/env/e1/services");
    const groups = screen.getAllByText(/^(Resources|Swarm)$/).map((g) => g.textContent);
    expect(groups).toEqual(["Resources", "Swarm"]);
  });

  it("lists Placement after Services, for a user who can read services", async () => {
    mount(1280, "/env/e1");
    const link = await screen.findByRole("link", { name: "Placement" });
    expect(link.getAttribute("href")).toBe("/env/e1/placement");
    const swarm = screen.getAllByRole("link").map((l) => l.textContent);
    expect(swarm.indexOf("Placement")).toBe(swarm.indexOf("Services") + 1);
  });

  it("is not there on a standalone environment", async () => {
    envType = "standalone";
    mount(1280, "/env/e1");
    expect(await screen.findByRole("link", { name: "Containers" })).toBeTruthy();
    // Give the environment list time to answer: the group must stay away once it has.
    await new Promise((r) => setTimeout(r, 50));
    expect(screen.queryByText("Swarm")).toBeNull();
    expect(screen.queryByRole("link", { name: "Services" })).toBeNull();
    expect(screen.queryByRole("link", { name: "Placement" })).toBeNull();
  });

  it("is not there for a user without services.read, though a swarm environment is open", async () => {
    me = makeMe({
      envs: { e1: "viewer" },
      permissions: splitPermissions(["containers.read", "environments.read"]),
    });
    mount(1280, "/env/e1");
    expect(await screen.findByRole("link", { name: "Containers" })).toBeTruthy();
    await new Promise((r) => setTimeout(r, 50));
    expect(screen.queryByRole("link", { name: "Services" })).toBeNull();
    expect(screen.queryByText("Swarm")).toBeNull();
  });
});
