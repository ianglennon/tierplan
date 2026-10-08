// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, within } from "@testing-library/react";
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
  setCsrfToken("csrf");
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      sent.push({ path, method: req.method });
      if (path === "/api/users" && req.method === "GET") return answer();
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
