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
    lastInstanceAdmin: false,
    createdAt: "2026-10-01T10:00:00Z",
    updatedAt: "2026-10-07T10:00:00.123456789Z",
    ...over,
  };
}

function memberBody(over: Record<string, unknown> = {}) {
  return {
    userId: "u1",
    username: "alice",
    displayName: "",
    instanceAdmin: false,
    status: "active",
    addedAt: "2026-10-07T10:00:00Z",
    ...over,
  };
}

function teamBody(over: Record<string, unknown> = {}) {
  const members = (over.members as unknown[] | undefined) ?? [];
  return {
    id: "t1",
    name: "ops",
    memberCount: members.length,
    grantCount: 0,
    createdAt: "2026-10-01T10:00:00Z",
    updatedAt: "2026-10-07T10:00:00.123456789Z",
    members,
    ...over,
  };
}

let current: Record<string, unknown>;
let team: Record<string, unknown>;
let users: Record<string, unknown>[];
let user: Record<string, unknown>;
let getTeam: () => Response | Promise<Response>;
let addMembers: (body: { userIds: string[] }) => Response | Promise<Response>;
let preview: (body: { op: string; userId?: string }) => Response | Promise<Response>;
let rename: (body: { name: string; updatedAt: string }) => Response | Promise<Response>;
let removeTeam: () => Response | Promise<Response>;
let removeMember: (userId: string) => Response | Promise<Response>;
let seen: string[];
let sent: Record<string, unknown>;

const defaultQueryOptions = queryClient.getDefaultOptions();

beforeEach(() => {
  queryClient.setDefaultOptions({ queries: { retry: false, refetchOnWindowFocus: false } });
  current = makeMe({ instanceAdmin: true, id: "me" });
  team = teamBody();
  users = [userBody(), userBody({ id: "u2", username: "bob", displayName: "Bob B" })];
  user = userBody();
  sent = {};
  seen = [];
  getTeam = () => Response.json(team);
  addMembers = (body) => {
    const added = users.filter((u) => body.userIds.includes(u.id as string));
    const members = [
      ...((team.members as unknown[]) ?? []),
      ...added.map((u) => memberBody({ userId: u.id, username: u.username })),
    ];
    team = teamBody({ members });
    return Response.json({ added: added.length });
  };
  preview = () => Response.json({ impact: [] });
  rename = (body) => {
    team = { ...team, name: body.name, updatedAt: "2026-10-08T10:00:00.5Z" };
    return Response.json(team);
  };
  removeTeam = () => Response.json({ impact: [] });
  removeMember = (userId) => {
    team = teamBody({
      members: ((team.members as { userId: string }[]) ?? []).filter((m) => m.userId !== userId),
    });
    return Response.json({ impact: [] });
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
      const body =
        req.method === "GET"
          ? undefined
          : await req
              .clone()
              .json()
              .catch(() => undefined);
      if (body !== undefined) sent[`${req.method} ${path}`] = body;
      if (path === "/api/users" && req.method === "GET") return Response.json(users);
      if (path === "/api/users/u1" && req.method === "GET") return Response.json(user);
      if (path === "/api/users/u1/sessions") return Response.json([]);
      if (path === "/api/teams" && req.method === "GET") {
        return Response.json([
          teamBody({ id: "t1", name: "ops" }),
          teamBody({ id: "t2", name: "dev" }),
        ]);
      }
      if (path === "/api/teams/t1" && req.method === "GET") return getTeam();
      if (path === "/api/teams/t1/impact" && req.method === "POST") {
        return preview(body as { op: string; userId?: string });
      }
      if (path === "/api/teams/t1" && req.method === "PATCH") {
        return rename(body as { name: string; updatedAt: string });
      }
      if (path === "/api/teams/t1" && req.method === "DELETE") return removeTeam();
      if (/^\/api\/teams\/t1\/members\/[^/]+$/.test(path) && req.method === "DELETE") {
        return removeMember(path.split("/").pop() as string);
      }
      if (path === "/api/teams/t1/members" && req.method === "POST") {
        return addMembers(body as { userIds: string[] });
      }
      if (path === "/api/teams/t2/members" && req.method === "POST") {
        user = userBody({ teams: [{ id: "t2", name: "dev" }], teamCount: 1 });
        return Response.json({ added: 1 });
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
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

const PAGE = "/settings/access/teams/t1";

describe("the Access page tabs", () => {
  it("shows the Users and Teams sub-tabs and opens the Teams table", async () => {
    renderAt("/settings/access/teams");
    const tabs = await screen.findByRole("tablist", { name: "Access sections" });
    expect(within(tabs).getByRole("tab", { name: "Users" })).toBeTruthy();
    const teamsTab = within(tabs).getByRole("tab", { name: "Teams" });
    expect(teamsTab.getAttribute("href") ?? teamsTab.querySelector("a")?.getAttribute("href")).toBe(
      "/settings/access/teams",
    );
    expect(await screen.findByRole("link", { name: "ops" })).toBeTruthy();
  });
});

describe("the team page", () => {
  it("shows the breadcrumb, the name as the title and the members with their status", async () => {
    team = teamBody({
      members: [
        memberBody(),
        memberBody({ userId: "u9", username: "root", instanceAdmin: true }),
        memberBody({ userId: "u3", username: "carol", status: "disabled" }),
      ],
    });
    renderAt(PAGE);
    const nav = await screen.findByRole("navigation", { name: "Breadcrumb" });
    expect(
      within(nav)
        .getAllByRole("listitem")
        .map((li) => li.textContent),
    ).toEqual(["Settings", "Access", "Teams", "ops"]);
    expect(screen.getByRole("heading", { level: 2, name: "ops" })).toBeTruthy();
    const table = screen.getByRole("table", { name: "Members of ops" });
    expect(within(table).getByRole("link", { name: "alice" }).getAttribute("href")).toBe(
      "/settings/access/users/u1",
    );
    const root = within(table).getByRole("link", { name: "root" }).closest("tr") as HTMLElement;
    expect(within(root).getByText("Instance admin")).toBeTruthy();
    const carol = within(table).getByRole("link", { name: "carol" }).closest("tr") as HTMLElement;
    expect(within(carol).getByText("Disabled")).toBeTruthy();
    expect(document.title).toBe("ops · Lab");
  });

  it("adds two users from the Add members popover: only users not in the team, checkmarks, one request", async () => {
    users = [
      userBody(),
      userBody({ id: "u2", username: "bob", displayName: "Bob B" }),
      userBody({ id: "u3", username: "carol" }),
    ];
    team = teamBody({ members: [memberBody()] });
    renderAt(PAGE);
    fireEvent.click(await screen.findByRole("button", { name: "Add members" }));

    const list = await screen.findByRole("listbox", { name: "Users" });
    // alice is in the team already.
    expect(await within(list).findByText("Bob B")).toBeTruthy();
    expect(within(list).queryByText("alice")).toBeNull();
    const add = screen.getByRole("button", { name: "Pick people to add" });
    expect(add.hasAttribute("disabled")).toBe(true);

    fireEvent.click(within(list).getByText("bob"));
    expect(await screen.findByRole("button", { name: "Add 1 member" })).toBeTruthy();
    fireEvent.click(within(list).getByText("carol"));
    fireEvent.click(await screen.findByRole("button", { name: "Add 2 members" }));

    expect(await screen.findByText("2 members added to ops.")).toBeTruthy();
    expect(sent["POST /api/teams/t1/members"]).toEqual({ userIds: ["u2", "u3"] });
    expect(await screen.findByRole("link", { name: "bob" })).toBeTruthy();
    expect(seen.filter((s) => s === "POST /api/teams/t1/members")).toHaveLength(1);
  });
});

describe("the Teams card on the user page", () => {
  const USER = "/settings/access/users/u1";

  it("lists the user's teams as links to their pages", async () => {
    user = userBody({ teams: [{ id: "t1", name: "ops" }], teamCount: 1 });
    renderAt(USER);
    const list = await screen.findByRole("list", { name: "Teams" });
    expect(within(list).getByRole("link", { name: "ops" }).getAttribute("href")).toBe(
      "/settings/access/teams/t1",
    );
  });

  it("says the user is in no team, and Add to team lists only the teams they are not in", async () => {
    user = userBody({ teams: [{ id: "t1", name: "ops" }], teamCount: 1 });
    renderAt(USER);
    await screen.findByRole("list", { name: "Teams" });
    fireEvent.click(screen.getByRole("button", { name: "Add to team" }));
    const options = await screen.findByRole("listbox", { name: "Teams" });
    expect(within(options).queryByText("ops")).toBeNull();
    fireEvent.click(await within(options).findByText("dev"));

    expect(await screen.findByText("alice added to dev.")).toBeTruthy();
    expect(sent["POST /api/teams/t2/members"]).toEqual({ userIds: ["u1"] });
    await waitFor(() => expect(screen.queryByRole("listbox", { name: "Teams" })).toBeNull());
  });

  it("shows the empty line for a user in no team", async () => {
    renderAt(USER);
    expect(await screen.findByText("alice isn't in any team.")).toBeTruthy();
  });
});

const IMPACT_ALICE = {
  userId: "u1",
  username: "alice",
  environmentId: "e1",
  environmentName: "Production",
  before: "operator",
  after: "viewer",
};

function problem(status: number, type: string) {
  return Response.json({ status, type, title: "x", detail: "x" }, { status });
}

describe("the team page states", () => {
  it("shows the header and card skeletons while the team loads", async () => {
    getTeam = () => new Promise<Response>(() => undefined);
    renderAt(PAGE);
    const busy = await screen.findByText("Loading team");
    expect((busy.parentElement as HTMLElement).getAttribute("aria-busy")).toBe("true");
  });

  it("shows the list error with Retry when the load fails, and loads on Retry", async () => {
    getTeam = () => Response.json({ status: 500, title: "x", detail: "x" }, { status: 500 });
    renderAt(PAGE);
    // One automatic retry a second later comes first.
    expect(await screen.findByText("Couldn't load teams.")).toBeTruthy();
    getTeam = () => Response.json(team);
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByRole("heading", { level: 2, name: "ops" })).toBeTruthy();
  });

  it("shows Page not found for an unknown team and does not retry the 404", async () => {
    getTeam = () => problem(404, "team_not_found");
    renderAt(PAGE);
    expect(await screen.findByText("Page not found")).toBeTruthy();
    expect(seen.filter((s) => s === "GET /api/teams/t1")).toHaveLength(1);
  });

  it("is refused for someone who does not hold teams.manage", async () => {
    current = makeMe({ envs: { e1: "admin" } });
    renderAt(PAGE);
    await waitFor(() => expect(screen.queryByRole("heading", { name: "ops" })).toBeNull());
    expect(seen).not.toContain("GET /api/teams/t1");
  });
});

describe("the Members card states", () => {
  it("says No members yet with Add members for a team with no members", async () => {
    renderAt(PAGE);
    expect(await screen.findByText("No members yet.")).toBeTruthy();
    expect(screen.queryByRole("table", { name: "Members of ops" })).toBeNull();
    expect(screen.getByRole("button", { name: "Add members" })).toBeTruthy();
  });

  it("shows the search above 10 members and not at 10, and filters by username and display name", async () => {
    const make = (n: number) =>
      Array.from({ length: n }, (_, i) =>
        memberBody({
          userId: `m${i}`,
          username: `member-${String(i).padStart(2, "0")}`,
          displayName: i === 3 ? "Zed Zimmer" : "",
        }),
      );
    team = teamBody({ members: make(10) });
    renderAt(PAGE);
    await screen.findByRole("table", { name: "Members of ops" });
    expect(screen.queryByRole("searchbox", { name: "Search members" })).toBeNull();
    cleanup();

    team = teamBody({ members: make(200) });
    renderAt(PAGE);
    const search = await screen.findByRole("searchbox", { name: "Search members" });
    // 200 members all in the table, which scrolls inside its own box.
    const table = screen.getByRole("table", { name: "Members of ops" });
    expect(within(table).getAllByRole("row")).toHaveLength(201);
    expect((table.parentElement as HTMLElement).className).toContain("overflow-y-auto");
    fireEvent.change(search, { target: { value: "zimmer" } });
    expect(within(table).getAllByRole("row")).toHaveLength(2);
    expect(within(table).getByRole("link", { name: "member-03" })).toBeTruthy();
    fireEvent.change(search, { target: { value: "nobody-like-this" } });
    expect(screen.getByText("No members match “nobody-like-this”.")).toBeTruthy();
  });

  it("scrolls the Add members list inside 320px", async () => {
    users = Array.from({ length: 200 }, (_, i) =>
      userBody({ id: `x${i}`, username: `person-${String(i).padStart(3, "0")}` }),
    );
    renderAt(PAGE);
    fireEvent.click(await screen.findByRole("button", { name: "Add members" }));
    const list = await screen.findByRole("listbox", { name: "Users" });
    await within(list).findByText("person-000");
    expect(list.className).toContain("max-h-80"); // 20rem, 320px
    expect(within(list).getAllByRole("option")).toHaveLength(200);
  });
});

describe("Remove from team", () => {
  beforeEach(() => {
    team = teamBody({ members: [memberBody(), memberBody({ userId: "u2", username: "bob" })] });
  });

  async function openRemove(username = "alice") {
    const row = (await screen.findByRole("link", { name: username })).closest("tr") as HTMLElement;
    const trigger = within(row).getByRole("button", { name: `Actions for ${username}` });
    fireEvent.pointerDown(trigger, { button: 0, ctrlKey: false });
    fireEvent.click(trigger);
    fireEvent.click(await screen.findByRole("menuitem", { name: "Remove from team…" }));
    return screen.findByRole("alertdialog", { name: `Remove ${username} from ops?` });
  }

  it("asks with focus on Cancel, shows the impact list, and keeps the confirm unusable until it has loaded", async () => {
    let finish: (r: Response) => void = () => {};
    preview = () => new Promise<Response>((r) => (finish = r));
    renderAt(PAGE);
    const dialog = await openRemove();
    expect(
      within(dialog).getByText(
        "alice loses the access this team gives them. They keep every other grant.",
      ),
    ).toBeTruthy();
    expect(document.activeElement).toBe(within(dialog).getByRole("button", { name: "Cancel" }));
    const confirm = within(dialog).getByRole("button", { name: "Remove from team" });
    expect(confirm.getAttribute("aria-disabled")).toBe("true");
    // Pressing a confirm that is not ready does nothing.
    fireEvent.click(confirm);
    expect(seen).not.toContain("DELETE /api/teams/t1/members/u1");

    finish(Response.json({ impact: [IMPACT_ALICE] }));
    expect(await within(dialog).findByText("Operator → Viewer")).toBeTruthy();
    await waitFor(() => expect(confirm.getAttribute("aria-disabled")).not.toBe("true"));
    expect(sent["POST /api/teams/t1/impact"]).toEqual({ op: "remove_member", userId: "u1" });
  });

  it("removes the member, says so, closes and refreshes the list", async () => {
    preview = () => Response.json({ impact: [IMPACT_ALICE] });
    renderAt(PAGE);
    const dialog = await openRemove();
    await within(dialog).findByText("Operator → Viewer");
    fireEvent.click(within(dialog).getByRole("button", { name: "Remove from team" }));
    expect(await screen.findByText("alice removed from ops.")).toBeTruthy();
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(seen.filter((s) => s === "DELETE /api/teams/t1/members/u1")).toHaveLength(1);
    await waitFor(() => expect(screen.queryByRole("link", { name: "alice" })).toBeNull());
    expect(screen.getByRole("link", { name: "bob" })).toBeTruthy();
  });

  it("fails closed when the preview fails, and a No one loses access preview keeps the confirm available", async () => {
    preview = () => Response.json({ status: 500, title: "x", detail: "x" }, { status: 500 });
    renderAt(PAGE);
    const dialog = await openRemove();
    expect(
      await within(dialog).findByText("Couldn't work out what changes. Try again."),
    ).toBeTruthy();
    const confirm = within(dialog).getByRole("button", { name: "Remove from team" });
    expect(confirm.getAttribute("aria-disabled")).toBe("true");

    preview = () => Response.json({ impact: [] });
    fireEvent.click(within(dialog).getByRole("button", { name: "Retry" }));
    expect((await within(dialog).findAllByText("No one loses access.")).length).toBeGreaterThan(0);
    await waitFor(() => expect(confirm.getAttribute("aria-disabled")).not.toBe("true"));
  });

  it("shows an inline alert, changes nothing and reads the team again when the removal fails", async () => {
    preview = () => Response.json({ impact: [IMPACT_ALICE] });
    removeMember = () => Response.json({ status: 500, title: "x", detail: "x" }, { status: 500 });
    renderAt(PAGE);
    const dialog = await openRemove();
    await within(dialog).findByText("Operator → Viewer");
    const before = seen.filter((s) => s === "GET /api/teams/t1").length;
    fireEvent.click(within(dialog).getByRole("button", { name: "Remove from team" }));
    const alert = await within(dialog).findByText(/Couldn't remove alice from ops/);
    expect(alert.closest('[role="alert"]')).toBeTruthy();
    expect(screen.getByRole("alertdialog")).toBeTruthy();
    await waitFor(() =>
      expect(seen.filter((s) => s === "GET /api/teams/t1").length).toBeGreaterThan(before),
    );
  });
});

describe("Delete team", () => {
  beforeEach(() => {
    team = teamBody({ members: [memberBody()] });
  });

  async function openDelete() {
    fireEvent.click(await screen.findByRole("button", { name: "Delete team…" }));
    return screen.findByRole("alertdialog", { name: "Delete team ops?" });
  }

  it("asks with focus on Cancel and no typed confirmation, lists the impact and waits for it", async () => {
    let finish: (r: Response) => void = () => {};
    preview = () => new Promise<Response>((r) => (finish = r));
    renderAt(PAGE);
    const dialog = await openDelete();
    expect(
      within(dialog).getByText(
        "Members lose the access this team gives them. They keep every other grant.",
      ),
    ).toBeTruthy();
    expect(document.activeElement).toBe(within(dialog).getByRole("button", { name: "Cancel" }));
    expect(within(dialog).queryByRole("textbox")).toBeNull();
    const confirm = within(dialog).getByRole("button", { name: "Delete team" });
    expect(confirm.getAttribute("aria-disabled")).toBe("true");

    finish(Response.json({ impact: [IMPACT_ALICE] }));
    expect(await within(dialog).findByText("Operator → Viewer")).toBeTruthy();
    await waitFor(() => expect(confirm.getAttribute("aria-disabled")).not.toBe("true"));
    expect(sent["POST /api/teams/t1/impact"]).toEqual({ op: "delete" });
  });

  it("deletes the team, says so and goes back to the Teams tab", async () => {
    preview = () => Response.json({ impact: [IMPACT_ALICE] });
    const router = renderAt(PAGE);
    const dialog = await openDelete();
    await within(dialog).findByText("Operator → Viewer");
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete team" }));
    expect(await screen.findByText("Team ops deleted.")).toBeTruthy();
    await waitFor(() => expect(router.state.location.pathname).toBe("/settings/access/teams"));
    expect(seen.filter((s) => s === "DELETE /api/teams/t1")).toHaveLength(1);
  });

  it("shows an inline alert in the dialog and stays put when the delete fails", async () => {
    removeTeam = () => Response.json({ status: 500, title: "x", detail: "x" }, { status: 500 });
    const router = renderAt(PAGE);
    const dialog = await openDelete();
    await within(dialog).findAllByText("No one loses access.");
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete team" }));
    const alert = await within(dialog).findByText(/Couldn't delete ops/);
    expect(alert.closest('[role="alert"]')).toBeTruthy();
    expect(router.state.location.pathname).toBe(PAGE);
    expect(screen.getByRole("alertdialog")).toBeTruthy();
  });
});

describe("the General card (rename)", () => {
  it("saves a new name with the version it read, says so and shows the new title", async () => {
    renderAt(PAGE);
    const name = await screen.findByLabelText("Team name");
    expect(screen.getByText("1 to 64 characters. Not case-sensitive.")).toBeTruthy();
    const save = screen.getByRole("button", { name: "Save changes" });
    expect(save.hasAttribute("disabled")).toBe(true);
    fireEvent.change(name, { target: { value: "  platform " } });
    fireEvent.click(save);
    expect(await screen.findByText("Team renamed.")).toBeTruthy();
    expect(sent["PATCH /api/teams/t1"]).toEqual({
      name: "platform",
      updatedAt: "2026-10-07T10:00:00.123456789Z",
    });
    expect(await screen.findByRole("heading", { level: 2, name: "platform" })).toBeTruthy();
  });

  it("asks for a name and says when it is taken", async () => {
    rename = () => problem(409, "team_name_taken");
    renderAt(PAGE);
    const name = await screen.findByLabelText("Team name");
    fireEvent.change(name, { target: { value: "" } });
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    expect(await screen.findByText("Enter a name.")).toBeTruthy();
    expect(sent["PATCH /api/teams/t1"]).toBeUndefined();

    fireEvent.change(name, { target: { value: "dev" } });
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    expect(await screen.findByText("That team name is already taken.")).toBeTruthy();
  });

  it("shows the stale-change alert, and an inline alert for any other failure", async () => {
    rename = () => problem(409, "stale_change");
    renderAt(PAGE);
    const name = await screen.findByLabelText("Team name");
    fireEvent.change(name, { target: { value: "platform" } });
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    const stale = await screen.findByText(
      "This changed while you were looking at it. Reload the page and try again.",
    );
    expect(stale.closest('[role="alert"]')).toBeTruthy();

    rename = () => Response.json({ status: 500, title: "x", detail: "x" }, { status: 500 });
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    const failed = await screen.findByText(/Couldn't rename the team/);
    expect(failed.closest('[role="alert"]')).toBeTruthy();
  });
});

describe("Remove from team on the user page", () => {
  it("removes the user from one of their teams after the impact list loads", async () => {
    user = userBody({
      teams: [
        { id: "t1", name: "ops" },
        { id: "t2", name: "dev" },
      ],
      teamCount: 2,
    });
    preview = () => Response.json({ impact: [IMPACT_ALICE] });
    renderAt("/settings/access/users/u1");
    const trigger = await screen.findByRole("button", { name: "Actions for ops" });
    fireEvent.pointerDown(trigger, { button: 0, ctrlKey: false });
    fireEvent.click(trigger);
    fireEvent.click(await screen.findByRole("menuitem", { name: "Remove from team…" }));
    const dialog = await screen.findByRole("alertdialog", { name: "Remove alice from ops?" });
    await within(dialog).findByText("Operator → Viewer");
    fireEvent.click(within(dialog).getByRole("button", { name: "Remove from team" }));
    expect(await screen.findByText("alice removed from ops.")).toBeTruthy();
    expect(seen).toContain("DELETE /api/teams/t1/members/u1");
  });
});
