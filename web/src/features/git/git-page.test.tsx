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
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { AppSidebar } from "@/components/app-sidebar";
import { SidebarProvider } from "@/components/ui/sidebar";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { GitPage, type GitSearch, type GitSearchPatch } from "@/features/git/git-page";
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
let credentials: unknown[];
let deleted: string[];
let deleteStatus: number;
let deleteBody: unknown;
let search: GitSearch;
let changes: GitSearchPatch[];

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
  credentials = [];
  deleted = [];
  deleteStatus = 204;
  deleteBody = undefined;
  search = {};
  changes = [];
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      if (url.pathname === "/api/me") return Response.json(me);
      if (req.method === "DELETE") {
        deleted.push(url.pathname);
        return deleteStatus === 204
          ? new Response(null, { status: 204 })
          : Response.json(deleteBody, { status: deleteStatus });
      }
      if (url.pathname === "/api/git/repos") return Response.json({ items: repos });
      if (url.pathname === "/api/git/credentials") return Response.json({ items: credentials });
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
  toast.dismiss();
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  client.clear();
});

function mount(tab: "repositories" | "credentials" = "repositories") {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <GitPage tab={tab} search={search} onSearchChange={(patch) => changes.push(patch)} />
          <Toaster />
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

const credential = (name: string, extra: Record<string, unknown> = {}) => ({
  id: name,
  name,
  scope: instance,
  type: "https",
  username: "deploy",
  last4: "wxyz",
  fingerprint: "",
  publicKey: "",
  readOnly: false,
  usedBy: 0,
  lastFailure: null,
  lastUseAt: null,
  createdAt: "2026-10-01T10:00:00Z",
  updatedAt: "2026-10-01T10:00:00Z",
  ...extra,
});

const openMenu = (name: string) => {
  const actions = screen.getByRole("button", { name: `Actions for ${name}` });
  fireEvent.pointerDown(actions, { button: 0, ctrlKey: false, pointerType: "mouse" });
  fireEvent.click(actions);
};

describe("GitPage credentials", () => {
  it("lists instance credentials first, then by name, with the columns of the UI-SPEC", async () => {
    credentials = [
      credential("zeta", {
        type: "ssh",
        last4: "",
        fingerprint: "SHA256:abcdefghijklmnopqrstuvwxyz",
        username: "git",
      }),
      credential("env-cred", { scope: lab }),
      credential("alpha", { usedBy: 2, lastUseAt: "2026-10-01T10:00:00Z" }),
      credential("refused", { lastFailure: { at: "2026-10-01T10:00:00Z", code: "denied" } }),
    ];
    repos = [
      repo("r-one", { credential: { id: "alpha", name: "alpha", scope: instance } }),
      repo("r-two", { credential: { id: "alpha", name: "alpha", scope: instance } }),
    ];
    mount("credentials");
    await screen.findByText("alpha");
    expect(bodyRows().map((r) => within(r).getAllByRole("cell")[0]?.textContent)).toEqual([
      "alpha",
      "refused",
      "zeta",
      "env-cred",
    ]);
    expect(screen.getAllByRole("columnheader").map((h) => h.textContent?.trim())).toEqual([
      "Name",
      "Scope",
      "Type",
      "Identity",
      "Secret",
      "Used by",
      "Last use",
      "Actions",
    ]);
    const alpha = bodyRows()[0] as HTMLElement;
    expect(within(alpha).getByText("HTTPS token")).toBeTruthy();
    expect(within(alpha).getByText("deploy")).toBeTruthy();
    expect(within(alpha).getByText("••••wxyz")).toBeTruthy();
    expect(within(alpha).getByText("2 repositories")).toBeTruthy();
    const zeta = bodyRows()[2] as HTMLElement;
    expect(within(zeta).getByText("SSH deploy key")).toBeTruthy();
    expect(within(zeta).getByText("Private key saved")).toBeTruthy();
    expect(within(zeta).getByText("SHA256:abcdefghijklmnopqrstuvwxyz")).toBeTruthy();
    expect(within(bodyRows()[1] as HTMLElement).getByText(/Refused/)).toBeTruthy();
    expect(screen.getByText("4 credentials")).toBeTruthy();
  });

  it("opens the Add credential form", async () => {
    credentials = [credential("alpha")];
    mount("credentials");
    await screen.findByText("alpha");
    fireEvent.click(screen.getByRole("button", { name: "Add credential" }));
    expect(changes).toEqual([{ credential: "new" }]);
  });

  it("lists the repositories that use a credential in the Used by popover", async () => {
    credentials = [credential("alpha", { usedBy: 2 })];
    repos = [
      repo("r-one", { credential: { id: "alpha", name: "alpha", scope: instance } }),
      repo("r-two", { credential: { id: "alpha", name: "alpha", scope: instance } }),
    ];
    mount("credentials");
    await screen.findByText("alpha");
    fireEvent.click(screen.getByRole("button", { name: "Used by alpha" }));
    expect(await screen.findByText("r-one")).toBeTruthy();
    expect(screen.getByText("r-two")).toBeTruthy();
  });
});

describe("GitPage delete", () => {
  it("asks 'Delete “name”?' with a destructive button and deletes the repository", async () => {
    repos = [repo("alpha")];
    mount();
    await screen.findByText("alpha");
    openMenu("alpha");
    fireEvent.click(await screen.findByRole("menuitem", { name: "Delete…" }));
    expect(await screen.findByText("Delete “alpha”?")).toBeTruthy();
    expect(
      screen.getByText(
        "Stacks can't be created from it any more. No stack uses it, so nothing else changes.",
      ),
    ).toBeTruthy();
    const confirm = screen.getByRole("button", { name: "Delete repository" });
    expect(confirm.className).toContain("bg-destructive-fill");
    fireEvent.click(confirm);
    await waitFor(() => expect(deleted).toEqual(["/api/git/repos/alpha"]));
    expect(await screen.findByText("Deleted repository alpha.")).toBeTruthy();
  });

  it("asks 'Delete “name”?' and deletes an environment credential through its route", async () => {
    credentials = [credential("lab-cred", { scope: lab })];
    mount("credentials");
    await screen.findByText("lab-cred");
    openMenu("lab-cred");
    fireEvent.click(await screen.findByRole("menuitem", { name: "Delete…" }));
    expect(await screen.findByText("Delete “lab-cred”?")).toBeTruthy();
    expect(screen.getByText("Nothing uses it, so no repository changes.")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Delete credential" }));
    await waitFor(() => expect(deleted).toEqual(["/api/env/e1/git/credentials/lab-cred"]));
  });

  it("keeps a credential in use from being deleted and says why", async () => {
    credentials = [credential("alpha", { usedBy: 2 })];
    mount("credentials");
    await screen.findByText("alpha");
    openMenu("alpha");
    const item = await screen.findByRole("menuitem", { name: "Delete…" });
    expect(item.getAttribute("aria-disabled")).toBe("true");
    expect(
      screen.getAllByText("Used by 2 repositories. Pick another credential for them first.").length,
    ).toBeGreaterThan(0);
    fireEvent.click(item);
    expect(screen.queryByText("Delete “alpha”?")).toBeNull();
    expect(deleted).toEqual([]);
  });

  it("keeps a repository in use from being deleted and says why", async () => {
    repos = [repo("alpha", { usedBy: 3 })];
    mount();
    await screen.findByText("alpha");
    openMenu("alpha");
    const item = await screen.findByRole("menuitem", { name: "Delete…" });
    expect(item.getAttribute("aria-disabled")).toBe("true");
    expect(
      screen.getAllByText("Used by 3 stacks. Point them at another repository first.").length,
    ).toBeGreaterThan(0);
    fireEvent.click(item);
    expect(deleted).toEqual([]);
  });

  it("shows the server's refusal inside the dialog when the credential became in use", async () => {
    credentials = [credential("alpha")];
    deleteStatus = 409;
    deleteBody = {
      title: "Conflict",
      status: 409,
      type: "credential_in_use",
      errors: [{ location: "conflict.count", message: "2" }],
    };
    mount("credentials");
    await screen.findByText("alpha");
    openMenu("alpha");
    fireEvent.click(await screen.findByRole("menuitem", { name: "Delete…" }));
    fireEvent.click(await screen.findByRole("button", { name: "Delete credential" }));
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain(
      "Used by 2 repositories. Pick another credential for them first.",
    );
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
