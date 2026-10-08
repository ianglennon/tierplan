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
