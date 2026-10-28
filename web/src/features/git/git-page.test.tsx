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
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { AppSidebar } from "@/components/app-sidebar";
import { SidebarProvider } from "@/components/ui/sidebar";
import { TooltipProvider } from "@/components/ui/tooltip";
import {
  GitPage,
  type RepositoriesSearch,
  type RepositoriesSearchPatch,
} from "@/features/git/git-page";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";
import { settled } from "@/test-settle";

// The switcher and the version footer have their own tests and their own endpoints; the sidebar
// test below is about which links it draws.
vi.mock("@/components/env-switcher", () => ({ EnvSwitcher: () => null }));
vi.mock("@/components/version-footer", () => ({ VersionFooter: () => null }));

let client: QueryClient;
let me: ReturnType<typeof makeMe>;
let repos: unknown[];
let search: RepositoriesSearch;
let changes: RepositoriesSearchPatch[];

const instance = { kind: "instance", envId: null, envName: null };
const lab = { kind: "environment", envId: "e1", envName: "Lab" };

const repo = (name: string, extra: Record<string, unknown> = {}) => ({
  id: name,
  name,
  scope: instance,
  url: "https://git.example.com/acme/stacks.git",
  host: "git.example.com",
  caPem: "",
  hasCa: false,
  credential: null,
  hostKey: null,
  lastTest: null,
  readOnly: false,
  usedBy: 0,
  createdAt: "2026-10-01T10:00:00Z",
  updatedAt: "2026-10-01T10:00:00Z",
  ...extra,
});

beforeEach(() => {
  me = makeMe({ instanceAdmin: true });
  repos = [];
  search = {};
  changes = [];
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      if (url.pathname === "/api/me") return Response.json(me);
      if (url.pathname === "/api/git/repos") return Response.json({ items: repos });
      if (url.pathname === "/api/git/credentials") return Response.json({ items: [] });
      if (url.pathname === "/api/environments") {
        return Response.json({
          items: [
            { id: "e1", name: "Lab", envType: "standalone" },
            { id: "e2", name: "Prod", envType: "swarm" },
          ],
        });
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  client.clear();
});

function mount() {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <GitPage search={search} onSearchChange={(patch) => changes.push(patch)} />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

const bodyRows = () => screen.getAllByRole("row").slice(1);

describe("GitPage repositories", () => {
  it("lists instance repositories first, then by name, with the columns of the UI-SPEC", async () => {
    repos = [
      repo("zeta"),
      repo("env-repo", { scope: lab }),
      repo("alpha", {
        url: "git@git.example.com:acme/stacks.git",
        credential: { id: "c1", name: "deploy-key", scope: instance },
        hostKey: {
          type: "ssh-ed25519",
          fingerprint: "SHA256:abcdefghijklmnop",
          trustedAt: "2026-10-01T10:00:00Z",
        },
        hasCa: true,
      }),
    ];
    mount();
    await screen.findByText("alpha");
    expect(bodyRows().map((r) => within(r).getAllByRole("cell")[0]?.textContent)).toEqual([
      "alpha",
      "zeta",
      "env-repo",
    ]);
    expect(screen.getAllByRole("columnheader").map((h) => h.textContent?.trim())).toEqual([
      "Name",
      "Scope",
      "Address",
      "Credential",
      "Host key",
      "Last test",
      "Used by",
      "Actions",
    ]);
    const alpha = bodyRows()[0] as HTMLElement;
    expect(within(alpha).getByText("Instance")).toBeTruthy();
    expect(within(alpha).getByText("git@git.example.com:acme/stacks.git")).toBeTruthy();
    expect(within(alpha).getByText("Custom CA")).toBeTruthy();
    expect(within(alpha).getByText("deploy-key")).toBeTruthy();
    expect(within(alpha).getByText("Trusted")).toBeTruthy();
    expect(within(alpha).getByText("SHA256:abcde")).toBeTruthy();
    expect(within(alpha).getByText("Not tested")).toBeTruthy();
    // A public repository over https has no credential and no host key.
    const zeta = bodyRows()[1] as HTMLElement;
    expect(within(zeta).getByText("None (public)")).toBeTruthy();
    expect(within(zeta).getByText("—")).toBeTruthy();
    expect(within(bodyRows()[2] as HTMLElement).getByText("Lab")).toBeTruthy();
    expect(screen.getByText("3 repositories")).toBeTruthy();
  });

  it("says Built-in for an ssh host with shipped keys and Not trusted yet for any other", async () => {
    repos = [
      repo("gh", { url: "git@github.com:acme/stacks.git" }),
      repo("own", { url: "ssh://git@git.example.com/acme/stacks.git" }),
    ];
    mount();
    await screen.findByText("gh");
    expect(within(bodyRows()[0] as HTMLElement).getByText("Built-in")).toBeTruthy();
    expect(within(bodyRows()[1] as HTMLElement).getByText("Not trusted yet")).toBeTruthy();
  });

  it("renders a name and an address that hold markup as text", async () => {
    repos = [repo("<img src=x onerror=alert(1)>", { url: "https://x.example/<b>a</b>.git" })];
    mount();
    expect(await screen.findByText("<img src=x onerror=alert(1)>")).toBeTruthy();
    expect(screen.getByText("https://x.example/<b>a</b>.git")).toBeTruthy();
    expect(document.querySelector("img")).toBeNull();
    expect(document.querySelector("b")).toBeNull();
  });

  it("opens the add form from Add repository", async () => {
    repos = [repo("alpha")];
    mount();
    await screen.findByText("alpha");
    fireEvent.click(screen.getByRole("button", { name: "Add repository" }));
    expect(changes).toEqual([{ repo: "new" }]);
  });
});

function mountSidebar() {
  const root = createRootRoute({
    component: () => (
      <TooltipProvider>
        <SidebarProvider>
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
      createRoute({ getParentRoute: () => root, path: "/git" }),
    ]),
    history: createMemoryHistory({ initialEntries: ["/"] }),
  });
  render(
    <QueryClientProvider client={client}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
}

describe("the sidebar item", () => {
  it("shows Git below Registries to a viewer of one environment", async () => {
    me = makeMe({ envs: { e1: "viewer" } });
    mountSidebar();
    const git = await screen.findByRole("link", { name: "Git" });
    const registries = screen.getByRole("link", { name: "Registries" });
    expect(registries.compareDocumentPosition(git) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(git.getAttribute("href")).toBe("/git");
  });

  it("does not show Git to someone with git.read nowhere", async () => {
    me = makeMe({});
    mountSidebar();
    await waitFor(() => expect(client.getQueryData(["me"])).toBeDefined());
    await settled(client);
    expect(screen.queryByRole("link", { name: "Git" })).toBeNull();
  });
});
