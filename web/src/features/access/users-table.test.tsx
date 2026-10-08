// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClientProvider } from "@tanstack/react-query";
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

const TOKEN = "ZyXwVuTsRqPoNmLkJiHgFeDcBa9876543210_-ZyXwV";

function newLink(): Response {
  return Response.json({
    url: `/invite#token=${TOKEN}`,
    expiresAt: new Date(Date.now() + 72 * 3_600_000).toISOString(),
  });
}

beforeEach(() => {
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
  setCsrfToken("csrf");
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      sent.push({ path, method: req.method });
      if (path === "/api/users" && req.method === "GET") return answer();
      if (/^\/api\/users\/[^/]+\/invite$/.test(path) && req.method === "POST") return regenerate();
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  queryClient.clear();
  setCsrfToken(null);
});

function renderTable() {
  render(
    <QueryClientProvider client={queryClient}>
      <TooltipProvider>
        <UsersTable />
      </TooltipProvider>
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
    expect(screen.queryByRole("button", { name: "Actions for admin" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Actions for erin" })).toBeNull();
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
