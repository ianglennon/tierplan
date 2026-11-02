// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { queryClient } from "@/api/query-client";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { routeTree } from "@/routeTree.gen";
import "@/i18n";
import { makeMe } from "@/test-me";

// The Access tab of an environment's settings page (plan 03-20, AUTH-06, D-06,
// D-12, D-25).

const ENV = {
  id: "e1",
  name: "prod",
  kind: "agent",
  enabled: true,
  isSelfHost: false,
  isLocalSocket: false,
  createdAt: "2026-10-01T10:00:00Z",
  engine: { name: "Docker Engine", version: "28.5.1", apiVersion: "1.50" },
  status: { state: "online" },
  envType: "swarm",
  agentVersion: null,
  agents: null,
  engineVersion: null,
  failingServices: null,
  lastSeen: null,
  nodes: null,
  updateAvailable: false,
};

function person(over: Record<string, unknown> = {}) {
  return {
    userId: "u-carol",
    username: "carol",
    displayName: "",
    role: "admin",
    extras: [],
    sources: [{ kind: "direct" }],
    grant: { id: "g-carol", role: "admin", extras: [] },
    instanceAdmin: false,
    self: true,
    ...over,
  };
}

const ADMIN_ROW = person({
  userId: "u-root",
  username: "root",
  extras: ["containers.exec", "secrets.reveal"],
  sources: [{ kind: "instance" }],
  grant: undefined,
  instanceAdmin: true,
  self: false,
});

let list: { teams: Array<Record<string, unknown>>; people: Array<Record<string, unknown>> };
let candidates: { users: Array<Record<string, unknown>>; teams: Array<Record<string, unknown>> };
let current: Record<string, unknown>;
let sent: Array<Record<string, unknown>>;
let putStatus: number;
let seen: string[];
let impactRows: Array<Record<string, unknown>>;
let impactStatus: number;
let removeStatus: number;
let candidatesPending: boolean;

const defaultQueryOptions = queryClient.getDefaultOptions();

beforeEach(() => {
  queryClient.setDefaultOptions({ queries: { retry: false, refetchOnWindowFocus: false } });
  list = { teams: [], people: [person(), ADMIN_ROW] };
  candidates = {
    users: [
      { id: "u-dave", username: "dave", displayName: "Dave Doe" },
      { id: "u-erin", username: "erin", displayName: "" },
    ],
    teams: [{ id: "t-ops", name: "ops" }],
  };
  current = makeMe({ username: "carol", id: "u-carol", envs: { e1: "admin" } });
  sent = [];
  putStatus = 201;
  seen = [];
  impactRows = [];
  impactStatus = 200;
  removeStatus = 200;
  candidatesPending = false;
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
      if (path === "/api/environments") return Response.json({ items: [ENV], total: 1 });
      if (path === "/api/env/e1/access" && req.method === "GET") return Response.json(list);
      if (path === "/api/env/e1/access/candidates") {
        if (candidatesPending) return new Promise<Response>(() => {});
        return Response.json(candidates);
      }
      if (path.endsWith("/impact") && req.method === "POST") {
        sent.push((await req.clone().json()) as Record<string, unknown>);
        return impactStatus === 200
          ? Response.json({ impact: impactRows })
          : Response.json({ title: "boom" }, { status: impactStatus });
      }
      if (path.startsWith("/api/env/e1/access/") && req.method === "DELETE") {
        return removeStatus === 200
          ? Response.json({ impact: impactRows })
          : Response.json({ title: "boom" }, { status: removeStatus });
      }
      if (path === "/api/env/e1/access" && req.method === "PUT") {
        const body = (await req.clone().json()) as Record<string, unknown>;
        sent.push(body);
        if (putStatus !== 201) return Response.json({ title: "boom" }, { status: putStatus });
        list = {
          ...list,
          people: [
            ...list.people,
            person({
              userId: body.subjectId,
              username: "dave",
              role: body.role,
              extras: body.extras,
              self: false,
              grant: { id: "g-new", role: body.role, extras: body.extras },
            }),
          ],
        };
        return Response.json(
          {
            id: "g-new",
            subjectType: body.subjectType,
            subjectId: body.subjectId,
            scope: "env",
            envId: "e1",
            envName: "prod",
            role: body.role,
            extras: body.extras,
            createdAt: "2026-10-08T10:00:00Z",
          },
          { status: 201 },
        );
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  toast.dismiss();
  queryClient.setDefaultOptions(defaultQueryOptions);
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

const ACCESS = "/settings/environments/e1/access";

async function tab() {
  const heading = await screen.findByRole("heading", { level: 3, name: "Access to prod" });
  return heading.closest("section") as HTMLElement;
}

describe("the environment page with its tab bar", () => {
  it("shows Settings / Environments / {name}, the tabs Settings and Access, and the Access tab selected from the URL", async () => {
    renderAt(ACCESS);
    await tab();
    const crumbs = within(screen.getByRole("navigation", { name: "Breadcrumb" }));
    expect(crumbs.getByText("prod")).toBeTruthy();
    const settingsTab = screen.getByRole("tab", { name: "Settings" });
    const accessTab = screen.getByRole("tab", { name: "Access" });
    expect(settingsTab.getAttribute("href")).toBe("/settings/environments/e1");
    expect(accessTab.getAttribute("href")).toBe("/settings/environments/e1/access");
    expect(accessTab.getAttribute("data-active")).toBe("true");
    expect(settingsTab.getAttribute("data-active")).toBeNull();
  });

  it("keeps the Settings cards on the Settings tab", async () => {
    renderAt("/settings/environments/e1");
    expect(await screen.findByRole("heading", { level: 3, name: "General" })).toBeTruthy();
    expect(screen.getByRole("tab", { name: "Access" })).toBeTruthy();
    expect(screen.getByRole("tab", { name: "Settings" }).getAttribute("data-active")).toBe("true");
  });

  it("hides the Access tab from someone who may configure but not give access", async () => {
    current = makeMe({
      username: "carol",
      id: "u-carol",
      permissions: { global: ["settings.read"], environments: { e1: ["environments.configure"] } },
    });
    renderAt("/settings/environments/e1");
    await screen.findByRole("heading", { level: 3, name: "General" });
    expect(screen.queryByRole("tab", { name: "Access" })).toBeNull();
  });

  it("shows no page at the Access address to someone without environments.access", async () => {
    current = makeMe({ username: "carol", id: "u-carol", envs: { e1: "operator" } });
    renderAt(ACCESS);
    expect(await screen.findByText("You don't have permission to view this page")).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "Access to prod" })).toBeNull();
  });
});

describe("the Access tab", () => {
  it("is titled Access to {name}, with its caption, a Teams table when teams hold access and the People table with the instance admin", async () => {
    list = {
      teams: [
        {
          teamId: "t-ops",
          name: "ops",
          role: "operator",
          extras: ["containers.exec"],
          sources: [{ kind: "direct" }],
          grant: { id: "g-ops", role: "operator", extras: ["containers.exec"] },
          members: [{ userId: "u-dave", username: "dave", role: "operator" }],
        },
      ],
      people: [person(), ADMIN_ROW],
    };
    renderAt(ACCESS);
    const c = await tab();
    expect(
      within(c).getByText(
        "Everyone who can use this environment and where their access comes from.",
      ),
    ).toBeTruthy();
    const teams = await within(c).findByRole("table", { name: "Teams with access to prod" });
    const ops = within(teams).getByText("ops").closest("tr") as HTMLElement;
    expect(within(ops).getByText("Operator")).toBeTruthy();
    expect(within(ops).getByText("Container shell")).toBeTruthy();
    expect(within(ops).getByText("Direct")).toBeTruthy();

    const people = within(c).getByRole("table", { name: "People with access to prod" });
    const carol = within(people).getByText("carol").closest("tr") as HTMLElement;
    expect(within(carol).getByText("Admin")).toBeTruthy();
    const root = within(people).getByText("root").closest("tr") as HTMLElement;
    expect(within(root).getByText("Instance admin")).toBeTruthy();
  });

  it("gives dave Operator with the container shell: the Who list offers teams and users, the dialog says On prod and the delegation limit, and the new person appears", async () => {
    renderAt(ACCESS);
    const c = await tab();
    await within(c).findByRole("table", { name: "People with access to prod" });
    fireEvent.click(within(c).getByRole("button", { name: "Add access" }));

    const dialog = await screen.findByRole("dialog", { name: "Add access" });
    // The environment is fixed, and an env Admin is told what they may give.
    expect(within(dialog).getByText("On prod")).toBeTruthy();
    expect(
      within(dialog).getByText(
        "You can give access on prod only. Instance admins and all-environments access are set by an instance admin.",
      ),
    ).toBeTruthy();
    expect(within(dialog).queryByText("All environments")).toBeNull();

    // Who: Teams then Users, never an email.
    fireEvent.click(await within(dialog).findByRole("combobox", { name: "Who" }));
    const list = await screen.findByRole("listbox");
    const groups = within(list)
      .getAllByRole("group")
      .map((g) => g.getAttribute("aria-labelledby"));
    expect(groups).toHaveLength(2);
    expect(
      within(list)
        .getAllByRole("option")
        .map((o) => o.textContent),
    ).toEqual(["ops", "dave" + "Dave Doe", "erin"]);
    fireEvent.click(within(list).getByRole("option", { name: /dave/ }));

    fireEvent.click(within(dialog).getByRole("radio", { name: /Operator/ }));
    fireEvent.click(within(dialog).getByRole("checkbox", { name: /Container shell/ }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Add access" }));

    expect(await screen.findByText("Access saved.")).toBeTruthy();
    expect(sent).toEqual([
      { subjectType: "user", subjectId: "u-dave", role: "operator", extras: ["containers.exec"] },
    ]);
    const people = within(c).getByRole("table", { name: "People with access to prod" });
    await waitFor(() => expect(within(people).getByText("dave")).toBeTruthy());
    const dave = within(people).getByText("dave").closest("tr") as HTMLElement;
    expect(within(dave).getByText("Operator")).toBeTruthy();
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("shows no delegation note to an instance admin giving access on the environment", async () => {
    current = makeMe({ instanceAdmin: true, username: "root", id: "u-root" });
    renderAt(ACCESS);
    const c = await tab();
    fireEvent.click(await within(c).findByRole("button", { name: "Add access" }));
    const dialog = await screen.findByRole("dialog", { name: "Add access" });
    expect(within(dialog).getByText("On prod")).toBeTruthy();
    expect(within(dialog).queryByText(/You can give access on prod only/)).toBeNull();
  });

  it("keeps the dialog open with the documented message when the server refuses the grant", async () => {
    putStatus = 403;
    renderAt(ACCESS);
    const c = await tab();
    fireEvent.click(await within(c).findByRole("button", { name: "Add access" }));
    const dialog = await screen.findByRole("dialog", { name: "Add access" });
    fireEvent.click(await within(dialog).findByRole("combobox", { name: "Who" }));
    fireEvent.click(await screen.findByRole("option", { name: /erin/ }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Add access" }));
    expect(await within(dialog).findByText("You can't give that access.")).toBeTruthy();
    expect(screen.getByRole("dialog", { name: "Add access" })).toBeTruthy();
  });
});

// The people and teams of a busier environment.
const OPS = {
  teamId: "t-ops",
  name: "ops",
  role: "operator",
  extras: ["containers.exec"],
  sources: [{ kind: "direct" }],
  grant: { id: "g-ops", role: "operator", extras: ["containers.exec"] },
  members: [
    { userId: "u-dave", username: "dave", displayName: "Dave Doe", role: "operator" },
    { userId: "u-erin", username: "erin", role: "operator" },
  ],
};
const DEV = {
  teamId: "t-dev",
  name: "dev",
  role: "viewer",
  extras: [],
  sources: [{ kind: "all" }],
  members: [{ userId: "u-gina", username: "gina", role: "viewer" }],
};

function rowsOf(table: HTMLElement): string[] {
  return within(table)
    .getAllByRole("row")
    .slice(1)
    .map((r) => r.textContent ?? "");
}

describe("the order and the markers of the rows", () => {
  it("sorts rows by role (Admin, Operator, Viewer) then name, and marks the signed-in user's own row with (you)", async () => {
    list = {
      teams: [],
      people: [
        person({ userId: "u-zed", username: "zed", role: "viewer", self: false, grant: undefined }),
        person({
          userId: "u-amy",
          username: "amy",
          role: "operator",
          self: false,
          grant: undefined,
        }),
        person({ userId: "u-bob", username: "bob", role: "viewer", self: false, grant: undefined }),
        ADMIN_ROW,
        person(),
      ],
    };
    renderAt(ACCESS);
    const c = await tab();
    const table = await within(c).findByRole("table", { name: "People with access to prod" });
    const names = within(table)
      .getAllByRole("row")
      .slice(1)
      .map((r) => within(r).getAllByRole("cell")[0]?.textContent);
    expect(names).toEqual(["carol (you)", "root", "amy", "bob", "zed"]);
    expect(within(table).getAllByText("(you)")).toHaveLength(1);
  });
});

describe("the Teams table", () => {
  it("says No teams have access to {name}. when none does, and still lists the People table", async () => {
    renderAt(ACCESS);
    const c = await tab();
    expect(await within(c).findByRole("heading", { level: 4, name: "Teams" })).toBeTruthy();
    expect(within(c).getByText("No teams have access to prod.")).toBeTruthy();
    expect(within(c).queryByRole("table", { name: "Teams with access to prod" })).toBeNull();
    expect(within(c).getByRole("table", { name: "People with access to prod" })).toBeTruthy();
  });

  it("expands a team row to list its members and the role each gets through the team", async () => {
    list = { teams: [OPS], people: [person(), ADMIN_ROW] };
    renderAt(ACCESS);
    const c = await tab();
    const teams = await within(c).findByRole("table", { name: "Teams with access to prod" });
    expect(within(teams).queryByText("erin")).toBeNull();
    const toggle = within(teams).getByRole("button", { name: "Show members of ops" });
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    fireEvent.click(toggle);
    expect(toggle.getAttribute("aria-label")).toBe("Hide members of ops");
    expect(toggle.getAttribute("aria-expanded")).toBe("true");
    const members = within(teams).getByRole("list", { name: "Members of ops" });
    const items = within(members).getAllByRole("listitem");
    expect(items.map((i) => i.textContent)).toEqual(["daveDave DoeOperator", "erinOperator"]);
    fireEvent.click(toggle);
    expect(within(teams).queryByRole("list", { name: "Members of ops" })).toBeNull();
  });
});

describe("the search", () => {
  beforeEach(() => {
    list = {
      teams: [OPS, DEV],
      people: [
        person(),
        ADMIN_ROW,
        person({
          userId: "u-dave",
          username: "dave",
          displayName: "Dave Doe",
          role: "operator",
          self: false,
          grant: undefined,
          sources: [{ kind: "team", teamId: "t-ops", teamName: "ops" }],
        }),
        person({
          userId: "u-erin",
          username: "erin",
          role: "operator",
          self: false,
          grant: undefined,
          sources: [{ kind: "team", teamId: "t-ops", teamName: "ops" }],
        }),
        person({
          userId: "u-gina",
          username: "gina",
          role: "viewer",
          self: false,
          grant: undefined,
          sources: [{ kind: "all" }],
        }),
      ],
    };
  });

  it("filters both tables at once by username, display name and team name, ignoring case", async () => {
    renderAt(ACCESS);
    const c = await tab();
    const search = await within(c).findByRole("searchbox", { name: "Search people and teams" });
    fireEvent.change(search, { target: { value: "DOE" } });
    const people = within(c).getByRole("table", { name: "People with access to prod" });
    expect(rowsOf(people)).toHaveLength(1);
    expect(within(people).getByText("dave")).toBeTruthy();
    // dave is a member of ops, so the team row matches through him and opens.
    const teams = within(c).getByRole("table", { name: "Teams with access to prod" });
    expect(within(teams).getByText("ops")).toBeTruthy();
    expect(within(teams).queryByText("dev")).toBeNull();

    fireEvent.change(search, { target: { value: "dev" } });
    expect(within(c).getByRole("table", { name: "Teams with access to prod" })).toBeTruthy();
    expect(within(c).getByText("No people match “dev”.")).toBeTruthy();
  });

  it("opens the team row whose member the search matches, and a name match alone leaves it closed", async () => {
    renderAt(ACCESS);
    const c = await tab();
    const search = await within(c).findByRole("searchbox", { name: "Search people and teams" });
    fireEvent.change(search, { target: { value: "ops" } });
    const teams = within(c).getByRole("table", { name: "Teams with access to prod" });
    expect(within(teams).queryByRole("list", { name: "Members of ops" })).toBeNull();

    fireEvent.change(search, { target: { value: "erin" } });
    expect(within(teams).getByRole("list", { name: "Members of ops" })).toBeTruthy();
    expect(within(teams).getByRole("button", { name: "Hide members of ops" })).toBeTruthy();
  });

  it("says when nothing matches in a table and shows everything again when the search is cleared", async () => {
    renderAt(ACCESS);
    const c = await tab();
    const search = await within(c).findByRole("searchbox", { name: "Search people and teams" });
    fireEvent.change(search, { target: { value: "zzz" } });
    expect(within(c).getByText("No teams match “zzz”.")).toBeTruthy();
    expect(within(c).getByText("No people match “zzz”.")).toBeTruthy();
    fireEvent.change(search, { target: { value: "" } });
    expect(
      rowsOf(within(c).getByRole("table", { name: "People with access to prod" })),
    ).toHaveLength(5);
    expect(
      rowsOf(within(c).getByRole("table", { name: "Teams with access to prod" })),
    ).toHaveLength(2);
  });
});

describe("the rows that are read only", () => {
  it("shows a lock with the reason in place of the menu for an instance admin and for access that comes from every environment or a team, and reads Admin, All, Instance admin for the instance admin", async () => {
    list = {
      teams: [DEV],
      people: [
        person(),
        ADMIN_ROW,
        person({
          userId: "u-gina",
          username: "gina",
          role: "viewer",
          self: false,
          grant: undefined,
          sources: [{ kind: "all" }],
        }),
      ],
    };
    renderAt(ACCESS);
    const c = await tab();
    const people = await within(c).findByRole("table", { name: "People with access to prod" });
    const root = within(people).getByText("root").closest("tr") as HTMLElement;
    expect(within(root).getByText("Admin")).toBeTruthy();
    expect(within(root).getByText("All")).toBeTruthy();
    expect(within(root).getByText("Instance admin")).toBeTruthy();
    const lock = within(root).getByRole("button", { name: "Actions for root" });
    expect(lock.getAttribute("aria-disabled")).toBe("true");
    const reason = lock.getAttribute("aria-describedby") as string;
    expect(document.getElementById(reason)?.textContent).toBe("Only an instance admin can do this");
    // Activating it explains itself and opens nothing.
    fireEvent.click(lock);
    expect(
      await screen.findByText("Only an instance admin can do this", {
        selector: "[data-sonner-toast] *",
      }),
    ).toBeTruthy();
    expect(screen.queryByRole("menuitem")).toBeNull();

    const gina = within(people).getByText("gina").closest("tr") as HTMLElement;
    expect(
      within(gina).getByRole("button", { name: "Actions for gina" }).getAttribute("aria-disabled"),
    ).toBe("true");
    const teams = within(c).getByRole("table", { name: "Teams with access to prod" });
    expect(
      within(teams).getByRole("button", { name: "Actions for dev" }).getAttribute("aria-disabled"),
    ).toBe("true");
  });
});

describe("editing and removing access", () => {
  async function openMenu(table: HTMLElement, who: string) {
    fireEvent.pointerDown(within(table).getByRole("button", { name: `Actions for ${who}` }), {
      button: 0,
      ctrlKey: false,
    });
    return screen.findByRole("menu");
  }

  it("offers Edit access and Remove access… on rows with a direct assignment here", async () => {
    list = { teams: [OPS], people: [person(), ADMIN_ROW] };
    renderAt(ACCESS);
    const c = await tab();
    const people = await within(c).findByRole("table", { name: "People with access to prod" });
    const menu = await openMenu(people, "carol");
    expect(
      within(menu)
        .getAllByRole("menuitem")
        .map((i) => i.textContent),
    ).toEqual(["Edit access", "Remove access…"]);
  });

  it("edits a grant: Who and the environment are text, a lowering shows what changes first, and the save carries the state it started from", async () => {
    list = {
      teams: [],
      people: [
        person({
          userId: "u-dave",
          username: "dave",
          role: "operator",
          self: false,
          extras: ["containers.exec"],
          grant: { id: "g-dave", role: "operator", extras: ["containers.exec"] },
        }),
        ADMIN_ROW,
      ],
    };
    impactRows = [
      {
        userId: "u-dave",
        username: "dave",
        environmentId: "e1",
        environmentName: "prod",
        before: "operator",
        after: "viewer",
      },
    ];
    renderAt(ACCESS);
    const c = await tab();
    const people = await within(c).findByRole("table", { name: "People with access to prod" });
    fireEvent.click(
      within(await openMenu(people, "dave")).getByRole("menuitem", { name: "Edit access" }),
    );

    const dialog = await screen.findByRole("dialog", { name: "Edit access" });
    expect(within(dialog).queryByRole("combobox", { name: "Who" })).toBeNull();
    expect(within(dialog).getByText("dave")).toBeTruthy();
    expect(within(dialog).getByText("prod")).toBeTruthy();
    expect(
      within(dialog)
        .getByRole("radio", { name: /Operator/ })
        .getAttribute("aria-checked"),
    ).toBe("true");

    fireEvent.click(within(dialog).getByRole("radio", { name: /Viewer/ }));
    // The preview comes from the environment's own route and Save waits for it.
    await within(dialog).findByText("What changes");
    await waitFor(() => expect(seen).toContain("POST /api/env/e1/access/g-dave/impact"));
    expect(await within(dialog).findByText("Operator → Viewer")).toBeTruthy();
    expect(sent).toContainEqual({ op: "update", role: "viewer", extras: [] });
    const save = within(dialog).getByRole("button", { name: "Save access" });
    await waitFor(() => expect((save as HTMLButtonElement).disabled).toBe(false));
    fireEvent.click(save);
    expect(await screen.findByText("Access saved.")).toBeTruthy();
    expect(sent).toContainEqual({
      subjectType: "user",
      subjectId: "u-dave",
      role: "viewer",
      extras: [],
      expected: { role: "operator", extras: ["containers.exec"] },
    });
  });

  it("removes access with the impact first: the preview comes from the environment's route and the confirm waits for it", async () => {
    list = {
      teams: [],
      people: [
        person(),
        person({
          userId: "u-dave",
          username: "dave",
          role: "viewer",
          self: false,
          extras: [],
          grant: { id: "g-dave", role: "viewer", extras: [] },
        }),
        ADMIN_ROW,
      ],
    };
    impactRows = [
      {
        userId: "u-dave",
        username: "dave",
        environmentId: "e1",
        environmentName: "prod",
        before: "viewer",
        after: "none",
      },
    ];
    renderAt(ACCESS);
    const c = await tab();
    const people = await within(c).findByRole("table", { name: "People with access to prod" });
    fireEvent.click(
      within(await openMenu(people, "dave")).getByRole("menuitem", { name: "Remove access…" }),
    );

    const dialog = await screen.findByRole("alertdialog", { name: "Remove access?" });
    expect(within(dialog).getByText("dave loses Viewer on prod.")).toBeTruthy();
    expect(await within(dialog).findByText("Viewer → No access")).toBeTruthy();
    expect(seen).toContain("POST /api/env/e1/access/g-dave/impact");
    expect(sent).toContainEqual({ op: "delete" });
    list = { teams: [], people: [person(), ADMIN_ROW] };
    fireEvent.click(within(dialog).getByRole("button", { name: "Remove access" }));
    expect(await screen.findByText("Access removed.")).toBeTruthy();
    expect(seen).toContain("DELETE /api/env/e1/access/g-dave");
    await waitFor(() => expect(within(c).queryByText("dave")).toBeNull());
  });

  it("warns an env Admin removing their own Admin grant, and the removal still goes ahead", async () => {
    renderAt(ACCESS);
    const c = await tab();
    const people = await within(c).findByRole("table", { name: "People with access to prod" });
    fireEvent.click(
      within(await openMenu(people, "carol")).getByRole("menuitem", { name: "Remove access…" }),
    );
    const dialog = await screen.findByRole("alertdialog", { name: "Remove access?" });
    expect(within(dialog).getByText("You will lose access to manage prod.")).toBeTruthy();
  });

  it("edits and removes a team's grant through the same menu", async () => {
    list = { teams: [OPS], people: [person(), ADMIN_ROW] };
    renderAt(ACCESS);
    const c = await tab();
    const teams = await within(c).findByRole("table", { name: "Teams with access to prod" });
    fireEvent.click(
      within(await openMenu(teams, "ops")).getByRole("menuitem", { name: "Edit access" }),
    );
    const dialog = await screen.findByRole("dialog", { name: "Edit access" });
    expect(within(dialog).getByText("ops")).toBeTruthy();
  });
});

describe("the Who list when there is nobody left or it has not loaded", () => {
  it("says Everyone already has access here. and keeps the button off when no user or team is left", async () => {
    candidates = { users: [], teams: [] };
    renderAt(ACCESS);
    const c = await tab();
    fireEvent.click(await within(c).findByRole("button", { name: "Add access" }));
    const dialog = await screen.findByRole("dialog", { name: "Add access" });
    expect(await within(dialog).findByText("Everyone already has access here.")).toBeTruthy();
    expect(within(dialog).queryByRole("combobox", { name: "Who" })).toBeNull();
    expect(
      (within(dialog).getByRole("button", { name: "Add access" }) as HTMLButtonElement).disabled,
    ).toBe(true);
  });

  it("shows one skeleton row in place of the Who field while the list loads", async () => {
    candidatesPending = true;
    renderAt(ACCESS);
    const c = await tab();
    fireEvent.click(await within(c).findByRole("button", { name: "Add access" }));
    const dialog = await screen.findByRole("dialog", { name: "Add access" });
    expect(await within(dialog).findByText("Loading users and teams")).toBeTruthy();
    expect(within(dialog).queryByRole("combobox", { name: "Who" })).toBeNull();
    expect(
      (within(dialog).getByRole("button", { name: "Add access" }) as HTMLButtonElement).disabled,
    ).toBe(true);
  });
});

describe("who sees Add access", () => {
  it("offers the button to an env Admin and to an instance admin, and no tab to an Operator", async () => {
    renderAt(ACCESS);
    expect(await screen.findByRole("button", { name: "Add access" })).toBeTruthy();
    cleanup();
    queryClient.clear();

    current = makeMe({ instanceAdmin: true, username: "root", id: "u-root" });
    renderAt(ACCESS);
    expect(await screen.findByRole("button", { name: "Add access" })).toBeTruthy();
    cleanup();
    queryClient.clear();

    current = makeMe({ username: "oscar", id: "u-oscar", envs: { e1: "operator" } });
    renderAt(ACCESS);
    await screen.findByText("You don't have permission to view this page");
    expect(screen.queryByRole("button", { name: "Add access" })).toBeNull();
  });
});

describe("many people", () => {
  it("scrolls both tables inside their card and keeps the header in view", async () => {
    list = {
      teams: [OPS],
      people: [
        person(),
        ADMIN_ROW,
        ...Array.from({ length: 100 }, (_, i) =>
          person({
            userId: `u-${i}`,
            username: `user${String(i).padStart(3, "0")}`,
            role: "viewer",
            self: false,
            grant: undefined,
            sources: [{ kind: "all" }],
          }),
        ),
      ],
    };
    renderAt(ACCESS);
    const c = await tab();
    for (const name of ["Teams with access to prod", "People with access to prod"]) {
      const table = await within(c).findByRole("table", { name });
      const box = table.closest("div[class*='overflow-auto']") as HTMLElement;
      expect(box.className).toContain("max-h-96");
      expect(within(table).getAllByRole("columnheader")[0]?.closest("thead")?.className).toContain(
        "sticky",
      );
    }
    expect(within(c).getAllByRole("row")).toHaveLength(1 + 1 + 102 + 1);
  });
});
