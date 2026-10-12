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
  RegistriesPage,
  type RegistriesSearch,
  type RegistriesSearchPatch,
} from "@/features/registries/registries-page";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// The switcher and the version footer have their own tests and their own endpoints; the
// sidebar tests below are about which links it draws.
vi.mock("@/components/env-switcher", () => ({ EnvSwitcher: () => null }));
vi.mock("@/components/version-footer", () => ({ VersionFooter: () => null }));

let client: QueryClient;
let me: ReturnType<typeof makeMe>;
let credentials: unknown[];
let listStatus: number;
let search: RegistriesSearch;
let changes: RegistriesSearchPatch[];

const credential = (id: string, extra: Record<string, unknown> = {}) => ({
  id,
  scope: { kind: "instance", envId: null, envName: null },
  name: id,
  type: "ghcr",
  host: "ghcr.io",
  prefix: "",
  username: "deploy",
  last4: "wxyz",
  hubFallback: false,
  caPem: "",
  hasCa: false,
  insecure: false,
  expiresAt: null,
  lastTest: null,
  lastFailure: null,
  canManage: false,
  createdAt: "2026-10-01T10:00:00Z",
  updatedAt: "2026-10-01T10:00:00Z",
  ...extra,
});

beforeEach(() => {
  me = makeMe({ envs: { e1: "viewer" } });
  credentials = [];
  listStatus = 200;
  search = {};
  changes = [];
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      if (url.pathname === "/api/me") return Response.json(me);
      if (url.pathname === "/api/registries") {
        return listStatus === 200
          ? Response.json({ items: credentials })
          : Response.json({ title: "boom", status: listStatus }, { status: listStatus });
      }
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
          <RegistriesPage search={search} onSearchChange={(patch) => changes.push(patch)} />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

const bodyRows = () => screen.getAllByRole("row").slice(1);

describe("RegistriesPage table", () => {
  it("lists instance credentials first, then by host and prefix, with the secret only as its last characters", async () => {
    me = makeMe({ instanceAdmin: true });
    credentials = [
      credential("zeta", { host: "registry.gitlab.com", type: "gitlab", last4: "" }),
      credential("env-cred", {
        scope: { kind: "environment", envId: "e1", envName: "Lab" },
        host: "a.example.com",
        type: "generic",
      }),
      credential("ghcr-acme", { prefix: "acme" }),
      credential("ghcr-all"),
    ];
    mount();
    await screen.findByText("ghcr-acme");
    expect(bodyRows().map((r) => within(r).getAllByRole("cell")[0]?.textContent)).toEqual([
      "ghcr-all",
      "ghcr-acme",
      "zeta",
      "env-cred",
    ]);
    const acme = bodyRows()[1] as HTMLElement;
    expect(within(acme).getByText("Instance")).toBeTruthy();
    expect(within(acme).getByText("GHCR")).toBeTruthy();
    expect(within(acme).getByText("ghcr.io/acme")).toBeTruthy();
    expect(within(acme).getByText("deploy")).toBeTruthy();
    expect(within(acme).getByText("••••wxyz")).toBeTruthy();
    expect(within(acme).getByText("Not tested")).toBeTruthy();
    // No expiry and no failure show a dash.
    expect(within(acme).getAllByText("—")).toHaveLength(2);
    // A secret the server kept no last characters for shows the dots alone.
    expect(within(bodyRows()[2] as HTMLElement).getByText("••••")).toBeTruthy();
    expect(within(bodyRows()[3] as HTMLElement).getByText("Lab")).toBeTruthy();
    expect(screen.getByText("4 credentials")).toBeTruthy();
  });

  it("marks the one Docker Hub fallback credential with All Hub pulls", async () => {
    credentials = [
      credential("hub", { type: "dockerhub", host: "docker.io", hubFallback: true }),
      credential("other"),
    ];
    mount();
    await screen.findByText("hub");
    expect(screen.getAllByText("All Hub pulls")).toHaveLength(1);
  });

  it("shows the last test and the sign-in failure", async () => {
    credentials = [
      credential("good", {
        lastTest: { at: "2026-10-01T10:00:00Z", ok: true, detail: "Signed in" },
      }),
      credential("bad", {
        lastTest: { at: "2026-10-01T10:00:00Z", ok: false, detail: "refused" },
        lastFailure: { at: "2026-10-01T11:00:00Z", code: "unauthorized" },
      }),
    ];
    mount();
    await screen.findByText("good");
    expect(screen.getByText("Passed")).toBeTruthy();
    expect(screen.getByText("Failed")).toBeTruthy();
    expect(screen.getByText("Sign-in failed")).toBeTruthy();
  });

  it("warns in the last 7 days and says Expired after", async () => {
    const day = 86_400_000;
    credentials = [
      credential("soon", { expiresAt: new Date(Date.now() + 3 * day).toISOString() }),
      credential("gone", { expiresAt: new Date(Date.now() - day).toISOString() }),
    ];
    mount();
    await screen.findByText("soon");
    expect(screen.getByText("Expires in 3 days")).toBeTruthy();
    expect(screen.getByText("Expired")).toBeTruthy();
  });

  it("renders a name that holds markup as text", async () => {
    credentials = [credential("x", { name: "<img src=x onerror=alert(1)>", username: "<b>u</b>" })];
    mount();
    expect(await screen.findByText("<img src=x onerror=alert(1)>")).toBeTruthy();
    expect(screen.getByText("<b>u</b>")).toBeTruthy();
    expect(document.querySelector("img")).toBeNull();
    expect(document.querySelector("b")).toBeNull();
  });

  it("shows 8 skeleton rows and the toolbar while it loads", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(() => new Promise(() => {})),
    );
    // /api/me is primed, so the page knows what the caller may do while the list loads.
    client.setQueryData(["me"], makeMe({ instanceAdmin: true }));
    mount();
    await waitFor(() => expect(screen.getAllByRole("row")).toHaveLength(9));
    expect(screen.getByRole("table").getAttribute("aria-busy")).toBe("true");
    expect(screen.getByRole("searchbox", { name: "Search credentials" })).toBeTruthy();
  });

  it("shows the error with Retry when the list cannot be read", async () => {
    listStatus = 500;
    mount();
    expect(await screen.findByText("Couldn't load credentials.")).toBeTruthy();
    expect(screen.getByText("The server may be busy. Try again.")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
  });

  it("filters by the search and the scope, and the caption counts them", async () => {
    credentials = [
      credential("alpha"),
      credential("beta", { scope: { kind: "environment", envId: "e1", envName: "Lab" } }),
    ];
    search = { q: "ALP" };
    mount();
    await screen.findByText("alpha");
    expect(screen.queryByText("beta")).toBeNull();
    expect(screen.getByText("1 of 2 credentials")).toBeTruthy();
  });

  it("filters to an environment's credentials", async () => {
    credentials = [
      credential("alpha"),
      credential("beta", { scope: { kind: "environment", envId: "e1", envName: "Lab" } }),
    ];
    search = { scope: "e1" };
    mount();
    await screen.findByText("beta");
    expect(screen.queryByText("alpha")).toBeNull();
  });
});

describe("RegistriesPage empty states", () => {
  it("offers Add credential to an instance administrator when there are none", async () => {
    me = makeMe({ instanceAdmin: true });
    mount();
    expect(await screen.findByText("No registry credentials")).toBeTruthy();
    expect(screen.getByText(/Add one so private images pull on every node/)).toBeTruthy();
    const buttons = screen.getAllByRole("button", { name: "Add credential" });
    fireEvent.click(buttons[buttons.length - 1] as HTMLElement);
    expect(changes).toEqual([{ credential: "new" }]);
  });

  it("tells everyone else to ask an administrator", async () => {
    mount();
    expect(await screen.findByText("No registry credentials apply here")).toBeTruthy();
    expect(screen.getByText(/Ask an administrator to add a credential/)).toBeTruthy();
  });

  it("says no credential matches the filters, with the way out", async () => {
    credentials = [credential("alpha")];
    search = { q: "zzz" };
    mount();
    expect(await screen.findByText("No credentials match these filters.")).toBeTruthy();
    fireEvent.click(screen.getAllByRole("button", { name: "Clear filters" }).at(-1) as HTMLElement);
    expect(changes).toContainEqual({ q: undefined, scope: undefined });
  });
});

describe("RegistriesPage permissions", () => {
  it("lets an instance administrator add a credential", async () => {
    me = makeMe({ instanceAdmin: true });
    credentials = [credential("alpha")];
    mount();
    await screen.findByText("alpha");
    const add = screen.getByRole("button", { name: "Add credential" });
    expect(add.getAttribute("aria-disabled")).toBeNull();
    fireEvent.click(add);
    expect(changes).toEqual([{ credential: "new" }]);
  });

  it("shows a viewer Add credential disabled with its reason, and View details only", async () => {
    credentials = [credential("alpha")];
    mount();
    await screen.findByText("alpha");
    const add = screen.getByRole("button", { name: "Add credential" });
    expect(add.getAttribute("aria-disabled")).toBe("true");
    expect(screen.getAllByText("Needs an instance administrator").length).toBeGreaterThan(0);
    fireEvent.click(add);
    expect(changes).toEqual([]);

    const actions = screen.getByRole("button", { name: "Actions for alpha" });
    fireEvent.pointerDown(actions, { button: 0, ctrlKey: false, pointerType: "mouse" });
    fireEvent.click(actions);
    const items = await screen.findAllByRole("menuitem");
    expect(items.map((i) => i.textContent)).toEqual(["View details"]);
    fireEvent.click(items[0] as HTMLElement);
    expect(changes).toEqual([{ credential: "alpha" }]);
  });

  it("enables Add credential for an environment Admin, who is not an instance administrator", async () => {
    me = makeMe({ envs: { e1: "admin" } });
    credentials = [credential("alpha")];
    mount();
    await screen.findByText("alpha");
    const add = screen.getByRole("button", { name: "Add credential" });
    expect(add.getAttribute("aria-disabled")).toBeNull();
    fireEvent.click(add);
    expect(changes).toEqual([{ credential: "new" }]);
  });

  it("lists only the environments the user can see in the Scope filter", async () => {
    me = makeMe({ envs: { e1: "viewer" } });
    credentials = [credential("alpha")];
    mount();
    await screen.findByText("alpha");
    fireEvent.keyDown(screen.getByRole("combobox", { name: "Scope" }), { key: "Enter" });
    const options = (await screen.findAllByRole("option")).map((o) => o.textContent);
    expect(options).toEqual(["All scopes", "Instance", "Lab"]);
  });

  it("gives each row the reason the user cannot manage it, and an environment Admin their own Edit and Delete…", async () => {
    me = makeMe({ envs: { e1: "admin", e2: "viewer" } });
    credentials = [
      credential("inst"),
      credential("lab", {
        scope: { kind: "environment", envId: "e1", envName: "Lab" },
        canManage: true,
      }),
      credential("prod", { scope: { kind: "environment", envId: "e2", envName: "Prod" } }),
    ];
    mount();
    await screen.findByText("lab");
    const open = (name: string) => {
      const actions = screen.getByRole("button", { name: `Actions for ${name}` });
      fireEvent.pointerDown(actions, { button: 0, ctrlKey: false, pointerType: "mouse" });
      fireEvent.click(actions);
    };
    // The reasons sit on the rows that cannot be managed, worded for their layer.
    expect(
      screen.getByRole("button", { name: "Actions for inst" }).getAttribute("aria-describedby"),
    ).toBeTruthy();
    expect(screen.getByText("Needs an instance administrator")).toBeTruthy();
    expect(screen.getByText("Needs Admin on Prod")).toBeTruthy();
    expect(screen.queryByText("Needs Admin on Lab")).toBeNull();
    expect(
      screen.getByRole("button", { name: "Actions for lab" }).getAttribute("aria-describedby"),
    ).toBeNull();

    open("lab");
    expect((await screen.findAllByRole("menuitem")).map((i) => i.textContent)).toEqual([
      "Edit",
      "Delete…",
    ]);
  });

  it("offers a manager Edit and a destructive Delete… after a separator", async () => {
    me = makeMe({ instanceAdmin: true });
    credentials = [credential("alpha", { canManage: true })];
    mount();
    await screen.findByText("alpha");
    const actions = screen.getByRole("button", { name: "Actions for alpha" });
    fireEvent.pointerDown(actions, { button: 0, ctrlKey: false, pointerType: "mouse" });
    fireEvent.click(actions);
    const items = await screen.findAllByRole("menuitem");
    expect(items.map((i) => i.textContent)).toEqual(["Edit", "Delete…"]);
    expect(screen.getAllByRole("separator").length).toBeGreaterThan(0);
    expect(items[1]?.getAttribute("data-variant")).toBe("destructive");
    fireEvent.click(items[0] as HTMLElement);
    expect(changes).toEqual([{ credential: "alpha" }]);
  });

  it("opens the delete confirmation from Delete… and removes the credential", async () => {
    me = makeMe({ instanceAdmin: true });
    credentials = [credential("alpha", { canManage: true })];
    const deleted: string[] = [];
    const base = globalThis.fetch;
    vi.stubGlobal(
      "fetch",
      vi.fn(async (req: Request) => {
        if (req.method === "DELETE") {
          deleted.push(new URL(req.url).pathname);
          credentials = [];
          return new Response(null, { status: 204 });
        }
        return base(req);
      }),
    );
    mount();
    await screen.findByText("alpha");
    const actions = screen.getByRole("button", { name: "Actions for alpha" });
    fireEvent.pointerDown(actions, { button: 0, ctrlKey: false, pointerType: "mouse" });
    fireEvent.click(actions);
    fireEvent.click(await screen.findByRole("menuitem", { name: "Delete…" }));
    expect(await screen.findByText("Delete “alpha”?")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Delete credential" }));
    await waitFor(() => expect(deleted).toEqual(["/api/registries/alpha"]));
    await waitFor(() => expect(screen.queryByText("Delete “alpha”?")).toBeNull());
    await waitFor(() => expect(screen.queryByRole("button", { name: "alpha" })).toBeNull());
  });

  it("opens a credential from its name", async () => {
    credentials = [credential("alpha")];
    mount();
    fireEvent.click(await screen.findByRole("button", { name: "alpha" }));
    expect(changes).toEqual([{ credential: "alpha" }]);
  });

  it("shows no permission to someone who reads images nowhere", async () => {
    me = makeMe({});
    mount();
    expect(await screen.findByText(/permission/i)).toBeTruthy();
    expect(screen.queryByRole("table")).toBeNull();
  });
});

// The sidebar reads the matched route's params, so it is drawn by a real (empty) route
// tree and not by the bare TestRouter.
describe("RegistriesPage sheet deep links", () => {
  it("opens the add sheet for ?credential=new for an instance administrator", async () => {
    me = makeMe({ instanceAdmin: true });
    credentials = [credential("alpha")];
    search = { credential: "new" };
    mount();
    expect(await screen.findByText("Add a registry credential")).toBeTruthy();
    expect(changes).toEqual([]);
  });

  it("drops ?credential=new for anyone else and shows the list", async () => {
    credentials = [credential("alpha")];
    search = { credential: "new" };
    mount();
    await screen.findByText("alpha");
    await waitFor(() => expect(changes).toContainEqual({ credential: undefined }));
    expect(screen.queryByText("Add a registry credential")).toBeNull();
  });

  it("opens ?credential={id} in view mode, and closing clears the parameter", async () => {
    credentials = [credential("alpha")];
    search = { credential: "alpha" };
    vi.stubGlobal(
      "fetch",
      vi.fn(async (req: Request) => {
        const url = new URL(req.url);
        if (url.pathname === "/api/me") return Response.json(me);
        if (url.pathname === "/api/registries/alpha") return Response.json(credential("alpha"));
        if (url.pathname === "/api/registries") return Response.json({ items: credentials });
        if (url.pathname === "/api/environments") return Response.json({ items: [] });
        return Response.json({}, { status: 404 });
      }),
    );
    mount();
    const sheet = await screen.findByRole("table", { name: "Credential details" });
    expect(within(sheet).getByText("••••wxyz")).toBeTruthy();
    expect(screen.getByText("You can see this credential but not change it.")).toBeTruthy();
    fireEvent.click(screen.getAllByRole("button", { name: "Close" })[0] as HTMLElement);
    expect(changes).toContainEqual({ credential: undefined });
  });
});

describe("RegistriesPage edit deep link", () => {
  it("opens ?credential={id} as the edit form for a credential the caller manages", async () => {
    me = makeMe({ instanceAdmin: true });
    credentials = [credential("alpha", { canManage: true })];
    search = { credential: "alpha" };
    const base = globalThis.fetch;
    vi.stubGlobal(
      "fetch",
      vi.fn(async (req: Request) => {
        if (new URL(req.url).pathname === "/api/registries/alpha") {
          return Response.json(credential("alpha", { canManage: true }));
        }
        return base(req);
      }),
    );
    mount();
    expect(await screen.findByText("Edit alpha")).toBeTruthy();
    expect(screen.getByLabelText("New secret (optional)")).toBeTruthy();
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
  it("shows Registries to a viewer of one environment, below Environments", async () => {
    mountSidebar();
    const link = await screen.findByRole("link", { name: "Registries" });
    const environments = screen.getByRole("link", { name: "Environments" });
    expect(
      environments.compareDocumentPosition(link) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
  });

  it("does not show Registries to someone with images.read nowhere", async () => {
    me = makeMe({});
    mountSidebar();
    await waitFor(() => expect(client.getQueryData(["me"])).toBeDefined());
    // Let the sidebar draw with what it has read.
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(screen.queryByRole("link", { name: "Registries" })).toBeNull();
    expect(screen.queryByRole("link", { name: "Environments" })).toBeNull();
  });
});
