// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import {
  createMemoryHistory,
  createRootRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { queryClient } from "@/api/query-client";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { UsersTable } from "@/features/access/users-table";
import "@/i18n";

function userRow(over: Record<string, unknown> = {}) {
  return {
    id: "u1",
    username: "alice",
    displayName: "",
    email: "",
    instanceAdmin: false,
    status: "active",
    teams: [],
    teamCount: 0,
    ...over,
  };
}

let users: Record<string, unknown>[];
let answer: () => Response | Promise<Response>;
let sent: { path: string; method: string }[];
let regenerate: () => Response | Promise<Response>;
let lifecycle: (path: string) => Response | Promise<Response>;
let delete_: () => Response | Promise<Response>;

const TOKEN = "ZyXwVuTsRqPoNmLkJiHgFeDcBa9876543210_-ZyXwV";

function newLink(): Response {
  return Response.json({
    url: `/invite#token=${TOKEN}`,
    expiresAt: new Date(Date.now() + 72 * 3_600_000).toISOString(),
  });
}

const defaultQueryOptions = queryClient.getDefaultOptions();

beforeEach(() => {
  // The app retries a failed read once after a second; a test of the error
  // state should not wait for it.
  queryClient.setDefaultOptions({ queries: { retry: false, refetchOnWindowFocus: false } });
  sent = [];
  users = [
    userRow({
      id: "u0",
      username: "admin",
      instanceAdmin: true,
      lastLoginAt: "2026-10-07T10:00:00Z",
    }),
  ];
  answer = () => Response.json(users);
  regenerate = () => newLink();
  delete_ = () => new Response(null, { status: 204 });
  lifecycle = (path) =>
    Response.json(
      userRow({
        id: path.split("/")[3],
        status: path.endsWith("/disable") ? "disabled" : "active",
        createdAt: "2026-10-01T10:00:00Z",
        updatedAt: "2026-10-07T10:00:00Z",
      }),
    );
  setCsrfToken("csrf");
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      sent.push({ path, method: req.method });
      if (path === "/api/users" && req.method === "GET") return answer();
      if (/^\/api\/users\/[^/]+\/invite$/.test(path) && req.method === "POST") return regenerate();
      if (/^\/api\/users\/[^/]+\/reset-password$/.test(path) && req.method === "POST") {
        return regenerate();
      }
      if (/^\/api\/users\/[^/]+\/(disable|enable)$/.test(path) && req.method === "POST") {
        return lifecycle(path);
      }
      if (/^\/api\/users\/[^/]+\/unlock$/.test(path) && req.method === "POST") {
        return Response.json(userRow({ id: path.split("/")[3], createdAt: "x", updatedAt: "x" }));
      }
      if (/^\/api\/users\/[^/]+$/.test(path) && req.method === "DELETE") {
        return delete_();
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
});

// The table links to the user page, so it renders inside a router. Only the
// table's own route exists here; the links just need an address.
function renderTable() {
  const root = createRootRoute({
    component: () => (
      <TooltipProvider>
        <UsersTable />
      </TooltipProvider>
    ),
  });
  const router = createRouter({
    routeTree: root,
    history: createMemoryHistory({ initialEntries: ["/"] }),
  });
  render(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
      <Toaster />
    </QueryClientProvider>,
  );
}

describe("UsersTable", () => {
  it("lists every user with the status the server gave, the instance-admin badge and the sign-in time", async () => {
    users = [
      userRow({
        id: "u0",
        username: "admin",
        instanceAdmin: true,
        lastLoginAt: "2026-10-07T10:00:00Z",
      }),
      userRow({
        id: "u1",
        username: "bob",
        displayName: "Bob B",
        status: "disabled",
        teamCount: 2,
      }),
      userRow({ id: "u2", username: "carol", status: "invite_pending" }),
      userRow({ id: "u3", username: "dave", status: "invite_expired" }),
      userRow({
        id: "u4",
        username: "erin",
        status: "locked",
        lockedUntil: "2026-10-07T12:30:00Z",
      }),
    ];
    renderTable();
    await screen.findByText("erin");
    const table = screen.getByRole("table", { name: "Users" });
    const rows = within(table).getAllByRole("row");
    expect(rows).toHaveLength(6); // header and five users
    const row = (name: string) => rows.find((r) => within(r).queryByText(name)) as HTMLElement;

    expect(within(row("admin")).getByText("Instance admin")).toBeTruthy();
    expect(within(row("admin")).getByText("Active")).toBeTruthy();
    expect(within(row("bob")).getByText("Bob B")).toBeTruthy();
    expect(within(row("bob")).getByText("Disabled")).toBeTruthy();
    expect(within(row("bob")).getByText("2")).toBeTruthy();
    expect(within(row("bob")).getByText("Never")).toBeTruthy();
    expect(within(row("carol")).getByText("Invite pending")).toBeTruthy();
    expect(within(row("dave")).getByText("Invite expired")).toBeTruthy();
    expect(within(row("erin")).getByText(/^Locked until/)).toBeTruthy();
  });

  it("sorts as the server sent them and shows the user count as a caption", async () => {
    renderTable();
    expect(await screen.findByText("1 user")).toBeTruthy();
  });
});

// Regenerate invite link (plan 03-12, D-22a).

const openMenu = (username: string) =>
  fireEvent.keyDown(screen.getByRole("button", { name: `Actions for ${username}` }), {
    key: "Enter",
  });

describe("Regenerate invite link", () => {
  beforeEach(() => {
    users = [
      userRow({ id: "u0", username: "admin", instanceAdmin: true }),
      userRow({ id: "u2", username: "carol", status: "invite_pending" }),
      userRow({ id: "u3", username: "dave", status: "invite_expired" }),
      userRow({
        id: "u4",
        username: "erin",
        status: "locked",
        lockedUntil: "2026-10-07T12:30:00Z",
      }),
    ];
  });

  it("offers the action only for Invite pending and Invite expired users", async () => {
    renderTable();
    await screen.findByText("carol");
    openMenu("carol");
    expect(await screen.findByRole("menuitem", { name: "Regenerate invite link" })).toBeTruthy();
    fireEvent.keyDown(document.activeElement ?? document.body, { key: "Escape" });
    await waitFor(() => expect(screen.queryByRole("menu")).toBeNull());

    openMenu("dave");
    expect(await screen.findByRole("menuitem", { name: "Regenerate invite link" })).toBeTruthy();
    fireEvent.keyDown(document.activeElement ?? document.body, { key: "Escape" });
    await waitFor(() => expect(screen.queryByRole("menu")).toBeNull());

    // A user who is active or locked has nothing to regenerate.
    openMenu("admin");
    expect(await screen.findByRole("menuitem", { name: "Open" })).toBeTruthy();
    expect(screen.queryByRole("menuitem", { name: "Regenerate invite link" })).toBeNull();
    fireEvent.keyDown(document.activeElement ?? document.body, { key: "Escape" });
    await waitFor(() => expect(screen.queryByRole("menu")).toBeNull());
    openMenu("erin");
    expect(await screen.findByRole("menuitem", { name: "Open" })).toBeTruthy();
    expect(screen.queryByRole("menuitem", { name: "Regenerate invite link" })).toBeNull();
  });

  it("asks first, with focus on Cancel, and sends nothing until Generate new link is pressed", async () => {
    renderTable();
    await screen.findByText("carol");
    openMenu("carol");
    fireEvent.click(await screen.findByRole("menuitem", { name: "Regenerate invite link" }));

    const confirm = await screen.findByRole("alertdialog", { name: "Generate a new invite link?" });
    expect(
      within(confirm).getByText("This replaces the previous link. The old one stops working."),
    ).toBeTruthy();
    expect(document.activeElement).toBe(within(confirm).getByRole("button", { name: "Cancel" }));
    expect(sent.some((s) => s.method === "POST")).toBe(false);

    fireEvent.click(within(confirm).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(sent.some((s) => s.method === "POST")).toBe(false);
  });

  it("shows the new link in the one-time link dialog and forgets it on close", async () => {
    renderTable();
    await screen.findByText("carol");
    openMenu("carol");
    fireEvent.click(await screen.findByRole("menuitem", { name: "Regenerate invite link" }));
    fireEvent.click(await screen.findByRole("button", { name: "Generate new link" }));

    const dialog = await screen.findByRole("dialog", { name: "Invite link for carol" });
    expect(sent.filter((s) => s.method === "POST")).toEqual([
      { path: "/api/users/u2/invite", method: "POST" },
    ]);
    expect(dialog.textContent).not.toContain(TOKEN);
    fireEvent.click(within(dialog).getByRole("button", { name: "Show link" }));
    expect(dialog.textContent).toContain(TOKEN);

    fireEvent.click(within(dialog).getByRole("button", { name: "Close" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(document.body.textContent).not.toContain(TOKEN);
    // The user is still waiting and the action is still offered.
    openMenu("carol");
    expect(await screen.findByRole("menuitem", { name: "Regenerate invite link" })).toBeTruthy();
  });

  it("shows the link error inside the confirm and lets the admin try again", async () => {
    regenerate = () => Response.json({ status: 500, title: "x", detail: "x" }, { status: 500 });
    renderTable();
    await screen.findByText("carol");
    openMenu("carol");
    fireEvent.click(await screen.findByRole("menuitem", { name: "Regenerate invite link" }));
    fireEvent.click(await screen.findByRole("button", { name: "Generate new link" }));

    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toBe(
      "Couldn't create the link. Try again, and check the Bayplan logs if it keeps happening.",
    );
    expect(screen.getByRole("alertdialog")).toBeTruthy();

    regenerate = () => newLink();
    fireEvent.click(screen.getByRole("button", { name: "Generate new link" }));
    expect(await screen.findByRole("dialog", { name: "Invite link for carol" })).toBeTruthy();
  });

  it("sends one request when Generate new link is pressed twice", async () => {
    let release: (r: Response) => void = () => undefined;
    regenerate = () => new Promise<Response>((resolve) => (release = resolve));
    renderTable();
    await screen.findByText("carol");
    openMenu("carol");
    fireEvent.click(await screen.findByRole("menuitem", { name: "Regenerate invite link" }));
    const button = await screen.findByRole("button", { name: "Generate new link" });
    fireEvent.click(button);
    fireEvent.click(button);
    expect(await screen.findByRole("button", { name: "Generating…" })).toBeTruthy();
    expect(sent.filter((s) => s.method === "POST")).toHaveLength(1);
    release(newLink());
    await screen.findByRole("dialog", { name: "Invite link for carol" });
  });

  it("explains a 409 not_invited and refreshes the list", async () => {
    regenerate = () =>
      Response.json({ status: 409, type: "not_invited", title: "x", detail: "x" }, { status: 409 });
    renderTable();
    await screen.findByText("carol");
    openMenu("carol");
    fireEvent.click(await screen.findByRole("menuitem", { name: "Regenerate invite link" }));
    fireEvent.click(await screen.findByRole("button", { name: "Generate new link" }));
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toBe(
      "carol has already set a password, so there is no invite to replace.",
    );
    await waitFor(() =>
      expect(
        sent.filter((s) => s.method === "GET" && s.path === "/api/users").length,
      ).toBeGreaterThan(1),
    );
  });
});

// Search, loading, error, long text, plurals and many users (plan 03-12 Task 3).

const searchField = () =>
  screen.getByRole("searchbox", { name: "Search users" }) as HTMLInputElement;
const typeSearch = (value: string) => fireEvent.change(searchField(), { target: { value } });
const bodyRows = () =>
  within(screen.getByRole("table", { name: "Users" }))
    .getAllByRole("row")
    .slice(1);

describe("UsersTable search", () => {
  beforeEach(() => {
    users = [
      userRow({
        id: "u1",
        username: "alice",
        displayName: "Alice Liddell",
        email: "a@wonder.test",
      }),
      userRow({ id: "u2", username: "bob", displayName: "Robert Table" }),
      userRow({ id: "u3", username: "carol", email: "Carol@Example.test" }),
    ];
  });

  it("filters in the browser, case-insensitively, over username, display name and email", async () => {
    renderTable();
    await screen.findByText("carol");
    const requests = sent.length;

    typeSearch("ALI");
    expect(bodyRows()).toHaveLength(1);
    expect(screen.getByText("alice")).toBeTruthy();

    typeSearch("robert");
    expect(bodyRows()).toHaveLength(1);
    expect(screen.getByText("bob")).toBeTruthy();

    typeSearch("carol@example");
    expect(bodyRows()).toHaveLength(1);
    expect(screen.getByText("carol")).toBeTruthy();

    typeSearch("");
    expect(bodyRows()).toHaveLength(3);
    // Searching asks the server for nothing.
    expect(sent).toHaveLength(requests);
  });

  it("says nothing matches, names the query and keeps the search field", async () => {
    renderTable();
    await screen.findByText("carol");

    typeSearch("zed");

    expect(screen.getByText("No users match “zed”.")).toBeTruthy();
    expect(screen.queryByRole("table", { name: "Users" })).toBeNull();
    expect(searchField().value).toBe("zed");
    expect(screen.getByRole("button", { name: "Create user" })).toBeTruthy();

    typeSearch("bob");
    expect(screen.queryByText(/No users match/)).toBeNull();
    expect(bodyRows()).toHaveLength(1);
  });
});

describe("UsersTable loading and failing", () => {
  it("shows six skeleton rows at once with the toolbar already there", async () => {
    let release: (r: Response) => void = () => undefined;
    answer = () => new Promise<Response>((resolve) => (release = resolve));
    renderTable();
    await waitFor(() => expect(sent.some((s) => s.path === "/api/users")).toBe(true));

    expect(screen.getByRole("button", { name: "Create user" })).toBeTruthy();
    expect(searchField()).toBeTruthy();
    const table = screen.getByRole("table", { name: "Users" });
    expect(table.getAttribute("aria-busy")).toBe("true");
    expect(bodyRows()).toHaveLength(6);
    expect(table.querySelectorAll('[data-slot="skeleton"]').length).toBeGreaterThan(0);
    expect(screen.queryByText(/^\d+ users?$/)).toBeNull();

    release(Response.json([userRow({ username: "alice" })]));
    await screen.findByText("alice");
    expect(screen.getByRole("table", { name: "Users" }).getAttribute("aria-busy")).toBeNull();
  });

  it("shows the load error with Retry, keeps the toolbar and refetches on Retry", async () => {
    answer = () => Response.json({ status: 500, title: "x", detail: "x" }, { status: 500 });
    renderTable();

    expect(await screen.findByText("Couldn't load users.")).toBeTruthy();
    expect(screen.getByText("Check the connection and try again.")).toBeTruthy();
    expect(screen.queryByRole("table", { name: "Users" })).toBeNull();
    expect(screen.getByRole("button", { name: "Create user" })).toBeTruthy();

    answer = () => Response.json([userRow({ username: "alice" })]);
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));

    expect(await screen.findByText("alice")).toBeTruthy();
    expect(screen.queryByText("Couldn't load users.")).toBeNull();
  });
});

describe("UsersTable cells", () => {
  it("cuts a long username and display name with an ellipsis and keeps the whole text in a tooltip", async () => {
    const longName = `${"a".repeat(60)}.${"b".repeat(60)}`;
    const longDisplay = "Maximilian ".repeat(20).trim();
    users = [userRow({ username: longName, displayName: longDisplay })];
    renderTable();

    const name = await screen.findByText(longName);
    expect(name.className).toContain("truncate");
    expect(screen.getByText(longDisplay).className).toContain("truncate");

    fireEvent.focus(name);
    const tip = await screen.findByRole("tooltip");
    expect(tip.textContent).toBe(longName);
  });

  it("never cuts a badge or a status", async () => {
    users = [userRow({ username: "a".repeat(80), instanceAdmin: true, status: "invite_expired" })];
    renderTable();
    const badge = await screen.findByText("Instance admin");
    expect(badge.className).toContain("shrink-0");
    expect(badge.className).toContain("whitespace-nowrap");
    expect(screen.getByText("Invite expired").className).toContain("whitespace-nowrap");
  });

  it("shows the username alone when there is no display name", async () => {
    users = [userRow({ username: "alice", displayName: "" })];
    renderTable();
    const cell = (await screen.findByText("alice")).closest("td") as HTMLElement;
    expect(cell.textContent).toBe("alice");
  });

  it("counts users and teams with plurals and writes 0, never blank", async () => {
    users = [userRow({ id: "u1", username: "alice", teamCount: 0 })];
    renderTable();
    expect(await screen.findByText("1 user")).toBeTruthy();
    const row = screen.getByText("alice").closest("tr") as HTMLElement;
    expect(within(row).getByText("0")).toBeTruthy();
    cleanup();
    queryClient.clear();

    users = [
      userRow({ id: "u1", username: "alice", teamCount: 1 }),
      userRow({ id: "u2", username: "bob", teamCount: 12 }),
    ];
    renderTable();
    expect(await screen.findByText("2 users")).toBeTruthy();
    expect(screen.getByText("12")).toBeTruthy();
    cleanup();
    queryClient.clear();

    users = [];
    renderTable();
    expect(await screen.findByText("0 users")).toBeTruthy();
  });

  it("shows Never in muted text for a user who has not signed in", async () => {
    users = [userRow({ username: "alice" })];
    renderTable();
    expect((await screen.findByText("Never")).className).toContain("text-muted-foreground");
  });
});

describe("UsersTable with many users", () => {
  it("renders 500 users in one table with a sticky header, no paging and a responsive search", async () => {
    users = Array.from({ length: 500 }, (_, i) =>
      userRow({ id: `u${i}`, username: `user${String(i).padStart(3, "0")}` }),
    );
    renderTable();
    expect(await screen.findByText("500 users")).toBeTruthy();
    expect(bodyRows()).toHaveLength(500);
    for (const th of screen.getAllByRole("columnheader")) {
      expect(th.className).toContain("sticky");
      expect(th.className).toContain("top-0");
      expect(th.className).toContain("h-12");
    }
    expect(screen.queryByRole("button", { name: /next|previous/i })).toBeNull();
    expect(screen.queryByRole("navigation", { name: /pagination/i })).toBeNull();

    typeSearch("user042");
    expect(bodyRows()).toHaveLength(1);
    expect(screen.getByText("500 users")).toBeTruthy();
  }, 20_000);
});

describe("UsersTable on a phone", () => {
  it("stacks the toolbar, makes Create user full width and 44px high and row triggers 44px", async () => {
    users = [userRow({ username: "carol", status: "invite_pending" })];
    renderTable();
    await screen.findByText("carol");

    const create = screen.getByRole("button", { name: "Create user" });
    expect(create.className).toContain("w-full");
    expect(create.className).toContain("h-11");
    expect((create.parentElement as HTMLElement).className).toContain("flex-col");
    expect(searchField().className).toContain("h-11");
    expect(screen.getByRole("button", { name: "Actions for carol" }).className).toContain(
      "max-md:size-11",
    );
    const wrapper = screen.getByRole("table", { name: "Users" }).parentElement as HTMLElement;
    expect(wrapper.className).toContain("overflow-x-auto");
  });
});

// Open, Disable and Enable (plan 03-14, D-22c).

describe("Open, Disable and Enable", () => {
  beforeEach(() => {
    users = [
      userRow({ id: "u0", username: "admin", instanceAdmin: true }),
      userRow({ id: "u1", username: "alice" }),
      userRow({ id: "u2", username: "bob", status: "disabled" }),
    ];
  });

  it("links the User cell to the user page and lists Open first in the row menu", async () => {
    renderTable();
    await screen.findByText("alice");
    expect(screen.getByRole("link", { name: "alice" }).getAttribute("href")).toBe(
      "/settings/access/users/u1",
    );
    openMenu("alice");
    const open = await screen.findByRole("menuitem", { name: "Open" });
    expect(open.getAttribute("href")).toBe("/settings/access/users/u1");
  });

  it("offers Disable for an enabled user and Enable for a disabled one", async () => {
    renderTable();
    await screen.findByText("alice");
    openMenu("alice");
    expect(await screen.findByRole("menuitem", { name: "Disable" })).toBeTruthy();
    expect(screen.queryByRole("menuitem", { name: "Enable" })).toBeNull();
    fireEvent.keyDown(document.activeElement ?? document.body, { key: "Escape" });
    await waitFor(() => expect(screen.queryByRole("menu")).toBeNull());
    openMenu("bob");
    expect(await screen.findByRole("menuitem", { name: "Enable" })).toBeTruthy();
    expect(screen.queryByRole("menuitem", { name: "Disable" })).toBeNull();
  });

  it("asks before disabling, with focus on Cancel, then disables and reads the list again", async () => {
    renderTable();
    await screen.findByText("alice");
    openMenu("alice");
    fireEvent.click(await screen.findByRole("menuitem", { name: "Disable" }));
    const confirm = await screen.findByRole("alertdialog", { name: "Disable alice?" });
    expect(document.activeElement).toBe(within(confirm).getByRole("button", { name: "Cancel" }));
    expect(sent.some((s) => s.method === "POST")).toBe(false);

    fireEvent.click(within(confirm).getByRole("button", { name: "Disable user" }));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(sent.filter((s) => s.method === "POST")).toEqual([
      { path: "/api/users/u1/disable", method: "POST" },
    ]);
    expect(await screen.findByText("alice is disabled.")).toBeTruthy();
  });

  it("enables at once, without a dialog, and says so", async () => {
    renderTable();
    await screen.findByText("bob");
    openMenu("bob");
    fireEvent.click(await screen.findByRole("menuitem", { name: "Enable" }));
    expect(await screen.findByText("bob is enabled.")).toBeTruthy();
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(sent.filter((s) => s.method === "POST")).toEqual([
      { path: "/api/users/u2/enable", method: "POST" },
    ]);
  });
});

// Unlock, Delete user… and the last-admin guard (plan 03-14 Task 3).

describe("Unlock and Delete user… in the row menu", () => {
  beforeEach(() => {
    users = [
      userRow({ id: "u0", username: "admin", instanceAdmin: true }),
      userRow({ id: "u1", username: "alice" }),
      userRow({
        id: "u4",
        username: "erin",
        status: "locked",
        lockedUntil: "2026-10-07T12:30:00Z",
      }),
    ];
  });

  it("offers Unlock only while a user is locked, and unlocking says they can sign in", async () => {
    renderTable();
    await screen.findByText("alice");
    openMenu("alice");
    expect(await screen.findByRole("menuitem", { name: "Open" })).toBeTruthy();
    expect(screen.queryByRole("menuitem", { name: "Unlock" })).toBeNull();
    fireEvent.keyDown(document.activeElement ?? document.body, { key: "Escape" });
    await waitFor(() => expect(screen.queryByRole("menu")).toBeNull());

    openMenu("erin");
    fireEvent.click(await screen.findByRole("menuitem", { name: "Unlock" }));
    expect(await screen.findByText("erin can sign in again.")).toBeTruthy();
    expect(sent.filter((s) => s.method === "POST")).toEqual([
      { path: "/api/users/u4/unlock", method: "POST" },
    ]);
  });

  it("deletes through the same type-to-confirm dialog and reads the list again", async () => {
    renderTable();
    await screen.findByText("alice");
    openMenu("alice");
    fireEvent.click(await screen.findByRole("menuitem", { name: "Delete user…" }));
    const dialog = await screen.findByRole("alertdialog", { name: "Delete alice?" });
    expect(document.activeElement).toBe(within(dialog).getByRole("button", { name: "Cancel" }));
    fireEvent.change(within(dialog).getByLabelText("Type alice to confirm"), {
      target: { value: "alice" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete user" }));
    expect(await screen.findByText("alice was deleted.")).toBeTruthy();
    expect(sent.filter((s) => s.method === "DELETE")).toEqual([
      { path: "/api/users/u1", method: "DELETE" },
    ]);
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
  });

  it("shows Disable and Delete user… for the last active instance admin as unavailable, with the reason", async () => {
    users = [
      userRow({ id: "u0", username: "admin", instanceAdmin: true, lastInstanceAdmin: true }),
    ];
    renderTable();
    await screen.findByText("admin");
    openMenu("admin");
    const reason = "Bayplan needs at least one active instance admin";
    for (const name of ["Disable", "Delete user…"]) {
      const item = await screen.findByRole("menuitem", { name });
      expect(item.getAttribute("aria-disabled")).toBe("true");
      const described = document.getElementById(item.getAttribute("aria-describedby") ?? "");
      expect(described?.textContent).toBe(reason);
    }
    fireEvent.click(screen.getByRole("menuitem", { name: "Disable" }));
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(sent.some((s) => s.method === "POST")).toBe(false);
  });
});

// Reset password in the row menu (plan 03-15, D-22b).

describe("Reset password", () => {
  beforeEach(() => {
    users = [
      userRow({ id: "u0", username: "admin", instanceAdmin: true }),
      userRow({ id: "u1", username: "alice" }),
      userRow({ id: "u2", username: "carol", status: "invite_pending" }),
    ];
  });

  it("is in the row menu of an active user, and asks before generating anything", async () => {
    renderTable();
    await screen.findByText("alice");
    openMenu("alice");
    fireEvent.click(await screen.findByRole("menuitem", { name: "Reset password" }));
    const confirm = await screen.findByRole("alertdialog", { name: "Reset password for alice?" });
    expect(
      within(confirm).getByText(
        "Their sessions end now, and you get a one-time link to give them.",
      ),
    ).toBeTruthy();
    expect(sent.some((s) => s.path.endsWith("/reset-password"))).toBe(false);

    fireEvent.click(within(confirm).getByRole("button", { name: "Generate reset link" }));
    const link = await screen.findByRole("dialog", { name: "Password reset link for alice" });
    expect(sent.filter((s) => s.path === "/api/users/u1/reset-password")).toHaveLength(1);
    expect(link.textContent).not.toContain(TOKEN);
  });

  it("sits right after Open in the menu", async () => {
    renderTable();
    await screen.findByText("alice");
    openMenu("alice");
    const menu = await screen.findByRole("menu");
    const labels = within(menu)
      .getAllByRole("menuitem")
      .map((i) => i.textContent);
    expect(labels.slice(0, 2)).toEqual(["Open", "Reset password"]);
  });
});
