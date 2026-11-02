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

// The Access card of a user's page (plan 03-19, AUTH-06, AUTH-07, D-06, D-25).

function userBody() {
  return {
    id: "u1",
    username: "alice",
    displayName: "",
    email: "",
    instanceAdmin: false,
    status: "active",
    teams: [],
    teamCount: 0,
    lastInstanceAdmin: false,
    createdAt: "2026-10-01T10:00:00Z",
    updatedAt: "2026-10-07T10:00:00.123456789Z",
  };
}

function envBody(id: string, name: string) {
  return { id, name, kind: "agent", enabled: true, status: { state: "online" } };
}

function grantBody(over: Record<string, unknown> = {}) {
  return {
    id: "g1",
    subjectType: "user",
    subjectId: "u1",
    scope: "env",
    envId: "env-a",
    envName: "Alpha",
    role: "viewer",
    extras: [],
    createdAt: "2026-10-08T10:00:00Z",
    ...over,
  };
}

function effectiveRow(over: Record<string, unknown> = {}) {
  return {
    envId: "env-a",
    envName: "Alpha",
    role: "viewer",
    extras: [],
    sources: [{ kind: "direct" }],
    inherited: false,
    ...over,
  };
}

let access: Record<string, unknown>;
let environments: Array<Record<string, unknown>>;
let putGrant: (body: Record<string, unknown>) => Response | Promise<Response>;
let sent: Record<string, unknown>;
let seen: string[];
let current: Record<string, unknown>;
let previewImpact: (body: Record<string, unknown>) => Response | Promise<Response>;
let removeGrant: () => Response | Promise<Response>;
let accessStatus: number;
let teamGrants: Array<Record<string, unknown>>;

const defaultQueryOptions = queryClient.getDefaultOptions();

beforeEach(() => {
  queryClient.setDefaultOptions({ queries: { retry: false, refetchOnWindowFocus: false } });
  access = { instanceAdmin: false, environments: [], grants: [] };
  current = makeMe({ instanceAdmin: true, id: "me" });
  accessStatus = 200;
  teamGrants = [];
  previewImpact = () => Response.json({ impact: [] });
  removeGrant = () => {
    access = { instanceAdmin: false, environments: [], grants: [] };
    return Response.json({ impact: [] });
  };
  environments = [envBody("env-b", "Beta"), envBody("env-a", "Alpha")];
  sent = {};
  seen = [];
  putGrant = (body) => {
    access = {
      instanceAdmin: false,
      environments: [effectiveRow({ role: body.role })],
      grants: [grantBody({ role: body.role })],
    };
    return Response.json(grantBody({ role: body.role }), { status: 201 });
  };
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
      if (path === "/api/users" && req.method === "GET") return Response.json([]);
      if (path === "/api/users" && req.method === "POST") {
        return Response.json(
          {
            user: { ...userBody(), id: "u9", username: "carol", status: "invite_pending" },
            link: {
              url: "/invite#token=AbCdEfGhIjKlMnOpQrStUvWxYz0123456789_-AbCde",
              expiresAt: new Date(Date.now() + 72 * 3_600_000).toISOString(),
            },
          },
          { status: 201 },
        );
      }
      if (path === "/api/users/u9") {
        return Response.json({ ...userBody(), id: "u9", username: "carol" });
      }
      if (path === "/api/users/u9/access") {
        return Response.json({ instanceAdmin: false, environments: [], grants: [] });
      }
      if (path === "/api/users/u9/sessions") return Response.json([]);
      if (path === "/api/users/u1") return Response.json(userBody());
      if (path === "/api/users/u1/sessions") return Response.json([]);
      if (path === "/api/users/u1/access") {
        return accessStatus === 200
          ? Response.json(access)
          : Response.json({ title: "boom" }, { status: accessStatus });
      }
      if (path === "/api/users/u1/grants/g1/impact") {
        return previewImpact((await req.clone().json()) as Record<string, unknown>);
      }
      if (path === "/api/users/u1/grants/g1" && req.method === "DELETE") return removeGrant();
      if (path === "/api/teams/t1") {
        return Response.json({
          id: "t1",
          name: "ops",
          memberCount: 0,
          grantCount: teamGrants.length,
          createdAt: "2026-10-01T10:00:00Z",
          updatedAt: "2026-10-07T10:00:00.123456789Z",
          members: [],
        });
      }
      if (path === "/api/teams/t1/grants" && req.method === "GET") return Response.json(teamGrants);
      if (path === "/api/teams/t1/grants" && req.method === "PUT") {
        const body = (await req.clone().json()) as Record<string, unknown>;
        sent[`${req.method} ${path}`] = body;
        teamGrants = [
          grantBody({ id: "tg1", subjectType: "team", subjectId: "t1", role: body.role }),
        ];
        return Response.json(teamGrants[0], { status: 201 });
      }
      if (path === "/api/environments") {
        return Response.json({ items: environments, total: environments.length });
      }
      if (path === "/api/users/u1/grants" && req.method === "PUT") {
        const body = (await req.clone().json()) as Record<string, unknown>;
        sent[`${req.method} ${path}`] = body;
        return putGrant(body);
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

const PAGE = "/settings/access/users/u1";

async function card() {
  const heading = await screen.findByRole("heading", { level: 3, name: "Access" });
  return heading.closest("section") as HTMLElement;
}

describe("the Access card of a user", () => {
  it("says the user has no access and offers Add access", async () => {
    renderAt(PAGE);
    const c = await card();
    expect(await within(c).findByText("alice has no access to any environment.")).toBeTruthy();
    expect(within(c).getByText("What alice can do on each environment, and why.")).toBeTruthy();
    expect(within(c).getByRole("button", { name: "Add access" })).toBeTruthy();
  });

  it("gives alice Viewer on an environment: the dialog sends the choice and the card shows the new row", async () => {
    renderAt(PAGE);
    const c = await card();
    await within(c).findByText("alice has no access to any environment.");
    fireEvent.click(within(c).getByRole("button", { name: "Add access" }));

    const dialog = await screen.findByRole("dialog", { name: "Add access" });
    // Environments are listed with All environments first, then alphabetically.
    const trigger = await within(dialog).findByRole("combobox", { name: "Environment" });
    await waitFor(() => expect((trigger as HTMLButtonElement).disabled).toBe(false));
    fireEvent.keyDown(trigger, { key: "Enter" });
    const options = (await screen.findAllByRole("option")).map((o) => o.textContent);
    expect(options).toEqual(["All environments", "Alpha", "Beta"]);
    fireEvent.keyDown(await screen.findByRole("option", { name: "Alpha" }), { key: "Enter" });

    // The picker offers the three roles with their descriptions, and Viewer is the default.
    expect(
      (within(dialog).getByRole("radio", { name: "Viewer" }) as HTMLButtonElement).getAttribute(
        "aria-checked",
      ),
    ).toBe("true");
    expect(within(dialog).getByRole("radio", { name: "Admin" })).toBeTruthy();
    expect(within(dialog).getByRole("radio", { name: "Operator" })).toBeTruthy();

    fireEvent.click(within(dialog).getByRole("button", { name: "Add access" }));
    expect(await screen.findByText("Access saved.")).toBeTruthy();
    expect(sent["PUT /api/users/u1/grants"]).toEqual({
      scope: "env",
      envId: "env-a",
      role: "viewer",
      extras: [],
    });

    // The effective table shows Alpha with a Viewer badge and the Direct source.
    const table = await within(c).findByRole("table", {
      name: "Access of alice by environment",
    });
    const row = within(table).getByText("Alpha").closest("tr") as HTMLElement;
    expect(within(row).getByText("Viewer")).toBeTruthy();
    expect(within(row).getByText("Direct")).toBeTruthy();
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("shows an instance admin as one summary row", async () => {
    access = { instanceAdmin: true, environments: [], grants: [] };
    renderAt(PAGE);
    const c = await card();
    expect(
      await within(c).findByText("Admin on every environment, with every extra permission"),
    ).toBeTruthy();
    expect(within(c).getByText("Instance admin")).toBeTruthy();
    expect(within(c).queryByText("alice has no access to any environment.")).toBeNull();
  });
});

describe("the effective access table", () => {
  it("shows the role, the extra permissions and every source in order, and marks an inherited environment", async () => {
    access = {
      instanceAdmin: false,
      environments: [
        effectiveRow({
          envId: "env-a",
          envName: "Alpha",
          role: "operator",
          extras: ["containers.exec", "secrets.reveal"],
          sources: [
            { kind: "all" },
            { kind: "team", teamId: "t1", teamName: "ops" },
            { kind: "direct" },
          ],
        }),
        effectiveRow({
          envId: "env-b",
          envName: "Beta",
          role: "viewer",
          sources: [{ kind: "all" }],
          inherited: true,
        }),
      ],
      grants: [grantBody({ role: "operator", extras: ["containers.exec"] })],
    };
    renderAt(PAGE);
    const c = await card();
    const table = await within(c).findByRole("table", { name: "Access of alice by environment" });
    expect(
      within(table)
        .getAllByRole("columnheader")
        .map((h) => h.textContent),
    ).toEqual(["Environment", "Role", "Extra permissions", "Comes from"]);

    const alpha = within(table).getByText("Alpha").closest("tr") as HTMLElement;
    expect(within(alpha).getByText("Operator")).toBeTruthy();
    expect(within(alpha).getByText("Container shell")).toBeTruthy();
    expect(within(alpha).getByText("Reveal secrets")).toBeTruthy();
    expect(within(alpha).queryByText("Inherited")).toBeNull();
    const sources = within(alpha)
      .getAllByRole("listitem")
      .map((li) => li.textContent)
      .filter((x) => ["All environments", "Team ops", "Direct"].includes(x ?? ""));
    expect(sources).toEqual(["All environments", "Team ops", "Direct"]);
    // The team badge goes to the team's page.
    expect(
      within(alpha).getByRole("link", { name: "Comes from Team ops" }).getAttribute("href"),
    ).toBe("/settings/access/teams/t1");

    const beta = within(table).getByText("Beta").closest("tr") as HTMLElement;
    expect(within(beta).getByText("Inherited")).toBeTruthy();
    expect(within(beta).getByText("Viewer")).toBeTruthy();
    expect(within(beta).getByText("None")).toBeTruthy(); // no extras is said, not left blank
    expect(within(beta).getByText("All environments")).toBeTruthy();
  });

  it("scrolls inside the card when the user reaches many environments", async () => {
    access = {
      instanceAdmin: false,
      environments: Array.from({ length: 100 }, (_, i) =>
        effectiveRow({ envId: `env-${i}`, envName: `Env ${String(i).padStart(3, "0")}` }),
      ),
      grants: [],
    };
    renderAt(PAGE);
    const c = await card();
    const table = await within(c).findByRole("table", { name: "Access of alice by environment" });
    expect(within(table).getAllByRole("row")).toHaveLength(101);
    const scroller = table.closest("div[class*='overflow-auto']") as HTMLElement;
    expect(scroller).toBeTruthy();
    expect(scroller.className).toContain("max-h-96");
  });

  it("shows skeleton rows while the access loads and an error with Retry when it cannot", async () => {
    accessStatus = 500;
    renderAt(PAGE);
    const c = await card();
    // The read is retried once after a second before it is called failed.
    expect(await within(c).findByText(/Couldn't load access\./)).toBeTruthy();
    accessStatus = 200;
    access = { instanceAdmin: false, environments: [effectiveRow()], grants: [] };
    fireEvent.click(within(c).getByRole("button", { name: "Retry" }));
    expect(await within(c).findByText("Alpha")).toBeTruthy();
  });
});

describe("direct access", () => {
  function withGrant(over: Record<string, unknown> = {}) {
    access = {
      instanceAdmin: false,
      environments: [effectiveRow({ role: over.role ?? "viewer" })],
      grants: [grantBody(over)],
    };
  }

  it("lists the user's own grants under Direct access, with Edit and Remove access in a row menu", async () => {
    withGrant({ role: "operator", extras: ["containers.exec"] });
    renderAt(PAGE);
    const c = await card();
    const direct = await within(c).findByRole("table", { name: "Direct access of alice" });
    expect(within(c).getByRole("heading", { level: 4, name: "Direct access" })).toBeTruthy();
    const row = within(direct).getByText("Alpha").closest("tr") as HTMLElement;
    expect(within(row).getByText("Operator")).toBeTruthy();
    expect(within(row).getByText("Container shell")).toBeTruthy();
    fireEvent.keyDown(within(row).getByRole("button", { name: "Actions for Alpha" }), {
      key: "Enter",
    });
    expect(await screen.findByRole("menuitem", { name: "Edit" })).toBeTruthy();
    expect(screen.getByRole("menuitem", { name: "Remove access…" })).toBeTruthy();
  });

  it("opens the edit dialog on the grant, and saving sends the version it started from", async () => {
    withGrant({ role: "operator", extras: ["containers.exec"] });
    putGrant = (body) => {
      access = {
        instanceAdmin: false,
        environments: [effectiveRow({ role: body.role })],
        grants: [grantBody({ role: body.role, extras: [] })],
      };
      return Response.json(grantBody({ role: body.role }));
    };
    renderAt(PAGE);
    const c = await card();
    const direct = await within(c).findByRole("table", { name: "Direct access of alice" });
    fireEvent.keyDown(within(direct).getByRole("button", { name: "Actions for Alpha" }), {
      key: "Enter",
    });
    fireEvent.click(await screen.findByRole("menuitem", { name: "Edit" }));
    const dialog = await screen.findByRole("dialog", { name: "Edit access" });
    fireEvent.click(within(dialog).getByRole("radio", { name: "Viewer" }));
    await within(dialog).findByText("No one loses access.");
    fireEvent.click(within(dialog).getByRole("button", { name: "Save access" }));
    await screen.findByText("Access saved.");
    expect(sent["PUT /api/users/u1/grants"]).toEqual({
      scope: "env",
      envId: "env-a",
      role: "viewer",
      extras: [],
      expected: { role: "operator", extras: ["containers.exec"] },
    });
    // The card shows the new role.
    await waitFor(() => {
      const row = within(c).getAllByText("Alpha")[0]?.closest("tr") as HTMLElement;
      expect(within(row).getByText("Viewer")).toBeTruthy();
    });
  });

  async function openRemove() {
    renderAt(PAGE);
    const c = await card();
    const direct = await within(c).findByRole("table", { name: "Direct access of alice" });
    fireEvent.keyDown(within(direct).getByRole("button", { name: "Actions for Alpha" }), {
      key: "Enter",
    });
    fireEvent.click(await screen.findByRole("menuitem", { name: "Remove access…" }));
    return { c, dialog: await screen.findByRole("alertdialog", { name: "Remove access?" }) };
  }

  it("asks Remove access? with what is lost, the impact list and a confirm that waits for it", async () => {
    withGrant({ role: "operator" });
    let release: (() => void) | undefined;
    previewImpact = () =>
      new Promise((resolve) => {
        release = () =>
          resolve(
            Response.json({
              impact: [
                {
                  userId: "u1",
                  username: "alice",
                  environmentId: "env-a",
                  environmentName: "Alpha",
                  before: "operator",
                  after: "none",
                },
              ],
            }),
          );
      });
    const { c, dialog } = await openRemove();
    expect(within(dialog).getByText("alice loses Operator on Alpha.")).toBeTruthy();
    // Focus starts on Cancel, and the destructive confirm is unusable until the preview loads.
    expect(document.activeElement).toBe(within(dialog).getByRole("button", { name: "Cancel" }));
    const confirm = within(dialog).getByRole("button", { name: "Remove access" });
    expect(confirm.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(confirm);
    expect(seen).not.toContain("DELETE /api/users/u1/grants/g1");

    release?.();
    expect(await within(dialog).findByText("Operator → No access")).toBeTruthy();
    await waitFor(() => expect(confirm.getAttribute("aria-disabled")).toBe("false"));
    expect(within(dialog).queryByText("You will lose access to manage Alpha.")).toBeNull();
    fireEvent.click(confirm);
    expect(await screen.findByText("Access removed.")).toBeTruthy();
    expect(seen).toContain("DELETE /api/users/u1/grants/g1");
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(await within(c).findByText("alice has no access to any environment.")).toBeTruthy();
  });

  it("warns a signed-in user who removes their own Admin that they lose access to manage the environment", async () => {
    current = makeMe({ instanceAdmin: true, id: "u1" });
    withGrant({ role: "admin" });
    const { dialog } = await openRemove();
    expect(await within(dialog).findByText("You will lose access to manage Alpha.")).toBeTruthy();
  });

  it("says so inside the dialog when the removal fails or the grant was already gone", async () => {
    withGrant({ role: "viewer" });
    removeGrant = () =>
      Response.json({ status: 500, type: "internal", title: "x" }, { status: 500 });
    const { dialog } = await openRemove();
    await within(dialog).findByText("No one loses access.");
    fireEvent.click(within(dialog).getByRole("button", { name: "Remove access" }));
    const alert = await within(dialog).findByRole("alert");
    expect(alert.textContent).toBe("Couldn't remove access. Try again.");
    expect(screen.getByRole("alertdialog", { name: "Remove access?" })).toBeTruthy();

    removeGrant = () =>
      Response.json({ status: 404, type: "grant_not_found", title: "x" }, { status: 404 });
    fireEvent.click(within(dialog).getByRole("button", { name: "Remove access" }));
    await waitFor(() =>
      expect(within(dialog).getByRole("alert").textContent).toBe(
        "That access was already removed. The list is up to date now.",
      ),
    );
  });
});

describe("the Access card of a team", () => {
  const TEAM = "/settings/access/teams/t1";

  it("sits between Members and Delete team, lists the team's grants and gives the team a role", async () => {
    renderAt(TEAM);
    await screen.findByRole("heading", { level: 2, name: "ops" });
    const titles = (await screen.findAllByRole("heading", { level: 3 })).map((h) => h.textContent);
    expect(titles).toEqual(["General", "Members", "Access", "Delete team"]);

    const c = (await screen.findByRole("heading", { level: 3, name: "Access" })).closest(
      "section",
    ) as HTMLElement;
    expect(await within(c).findByText("ops has no access to any environment yet.")).toBeTruthy();
    fireEvent.click(within(c).getByRole("button", { name: "Add access" }));
    const dialog = await screen.findByRole("dialog", { name: "Add access" });
    const trigger = await within(dialog).findByRole("combobox", { name: "Environment" });
    await waitFor(() => expect((trigger as HTMLButtonElement).disabled).toBe(false));
    fireEvent.keyDown(trigger, { key: "Enter" });
    fireEvent.keyDown(await screen.findByRole("option", { name: "Alpha" }), { key: "Enter" });
    fireEvent.click(within(dialog).getByRole("radio", { name: "Operator" }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Add access" }));
    expect(await screen.findByText("Access saved.")).toBeTruthy();
    expect(sent["PUT /api/teams/t1/grants"]).toEqual({
      scope: "env",
      envId: "env-a",
      role: "operator",
      extras: [],
    });
    const table = await within(c).findByRole("table", { name: "Direct access of ops" });
    expect(within(table).getByText("Alpha")).toBeTruthy();
    expect(within(table).getByText("Operator")).toBeTruthy();
  });
});

describe("Set up access after creating a user", () => {
  it("opens the new user's page at the Access card", async () => {
    const router = renderAt("/settings/access/users");
    fireEvent.click(await screen.findByRole("button", { name: "Create user" }));
    const dialog = await screen.findByRole("dialog", { name: "Create user" });
    fireEvent.change(within(dialog).getByLabelText("Username"), { target: { value: "carol" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Create user" }));
    const ready = await screen.findByRole("dialog", { name: "carol is ready" });
    fireEvent.click(within(ready).getByRole("button", { name: "Set up access" }));

    await waitFor(() => expect(router.state.location.pathname).toBe("/settings/access/users/u9"));
    expect(router.state.location.hash).toBe("access");
    const c = await card();
    expect(await within(c).findByText("carol has no access to any environment.")).toBeTruthy();
    expect(screen.queryByRole("dialog")).toBeNull();
  });
});
