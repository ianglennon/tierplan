// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

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
import { TeamsTable } from "@/features/access/teams-table";
import "@/i18n";

function teamRow(over: Record<string, unknown> = {}) {
  return {
    id: "t1",
    name: "ops",
    memberCount: 0,
    grantCount: 0,
    createdAt: "2026-10-01T10:00:00Z",
    updatedAt: "2026-10-07T10:00:00.123456789Z",
    ...over,
  };
}

let teams: Record<string, unknown>[];
let answer: () => Response | Promise<Response>;
let create: (body: { name: string }) => Response | Promise<Response>;
let sent: { path: string; method: string; body?: unknown }[];
let preview: () => Response | Promise<Response>;
let rename: (body: { name: string; updatedAt: string }) => Response | Promise<Response>;
let remove: () => Response | Promise<Response>;

const defaultQueryOptions = queryClient.getDefaultOptions();

beforeEach(() => {
  // The app retries a failed read once after a second; a test of an error state
  // should not wait for it.
  queryClient.setDefaultOptions({ queries: { retry: false, refetchOnWindowFocus: false } });
  sent = [];
  teams = [];
  answer = () => Response.json(teams);
  create = (body) => {
    const team = teamRow({ id: "t-new", name: body.name });
    teams = [...teams, team];
    return Response.json(team, { status: 201 });
  };
  preview = () => Response.json({ impact: [] });
  rename = (body) => Response.json(teamRow({ name: body.name }));
  remove = () => Response.json({ impact: [] });
  setCsrfToken("csrf");
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      const body =
        req.method === "GET"
          ? undefined
          : await req
              .clone()
              .json()
              .catch(() => undefined);
      sent.push({ path, method: req.method, body });
      if (path === "/api/teams" && req.method === "GET") return answer();
      if (path === "/api/teams" && req.method === "POST") return create(body as { name: string });
      if (/^\/api\/teams\/[^/]+\/impact$/.test(path)) return preview();
      if (/^\/api\/teams\/[^/]+$/.test(path) && req.method === "PATCH") {
        return rename(body as { name: string; updatedAt: string });
      }
      if (/^\/api\/teams\/[^/]+$/.test(path) && req.method === "DELETE") return remove();
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

// The table links to the team page, so it renders inside a router. Only the
// table's own route exists here; the links just need an address.
function renderTable() {
  const root = createRootRoute({
    component: () => (
      <TooltipProvider>
        <TeamsTable />
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

describe("TeamsTable", () => {
  it("lists every team with its members and what it grants, and links to the team page", async () => {
    teams = [
      teamRow({ id: "t1", name: "ops", memberCount: 2, grantCount: 3 }),
      teamRow({ id: "t2", name: "dev", memberCount: 1, grantCount: 0 }),
    ];
    renderTable();
    const link = await screen.findByRole("link", { name: "ops" });
    expect(link.getAttribute("href")).toBe("/settings/access/teams/t1");
    const row = link.closest("tr") as HTMLElement;
    expect(within(row).getByText("2")).toBeTruthy();
    expect(within(row).getByText("3 grants")).toBeTruthy();
    const dev = screen.getByRole("link", { name: "dev" }).closest("tr") as HTMLElement;
    expect(within(dev).getByText("No access")).toBeTruthy();
    expect(screen.getByText("2 teams")).toBeTruthy();
  });

  it("creates a team from the dialog, closes it, says so and lists the new team", async () => {
    renderTable();
    fireEvent.click(await screen.findByRole("button", { name: "Create team" }));
    const dialog = await screen.findByRole("dialog", { name: "Create team" });
    expect(within(dialog).getByText("1 to 64 characters. Not case-sensitive.")).toBeTruthy();
    fireEvent.change(within(dialog).getByLabelText("Team name"), { target: { value: "  ops  " } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Create team" }));

    expect(await screen.findByText("ops created.")).toBeTruthy();
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(sent.find((s) => s.method === "POST")?.body).toEqual({ name: "ops" });
    expect(await screen.findByRole("link", { name: "ops" })).toBeTruthy();
  });

  it("asks for a name and says when the name is taken, keeping the dialog open", async () => {
    create = () =>
      Response.json(
        { status: 409, type: "team_name_taken", title: "Conflict", detail: "x" },
        { status: 409 },
      );
    renderTable();
    fireEvent.click(await screen.findByRole("button", { name: "Create team" }));
    const dialog = await screen.findByRole("dialog", { name: "Create team" });

    fireEvent.click(within(dialog).getByRole("button", { name: "Create team" }));
    expect(await within(dialog).findByText("Enter a name.")).toBeTruthy();
    expect(sent.some((s) => s.method === "POST")).toBe(false);

    fireEvent.change(within(dialog).getByLabelText("Team name"), { target: { value: "Ops" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Create team" }));
    expect(await within(dialog).findByText("That team name is already taken.")).toBeTruthy();
    expect(screen.getByRole("dialog", { name: "Create team" })).toBeTruthy();
  });
});

describe("TeamsTable states and row actions", () => {
  it("shows No teams yet with the documented body and Create team when there are none", async () => {
    renderTable();
    expect(await screen.findByText("No teams yet")).toBeTruthy();
    expect(
      screen.getByText(
        "Teams give the same access to several people at once. Create a team, add people, then give it a role on an environment.",
      ),
    ).toBeTruthy();
    // One Create team in the toolbar and one in the empty state.
    expect(screen.getAllByRole("button", { name: "Create team" }).length).toBeGreaterThan(0);
    expect(screen.queryByRole("table", { name: "Teams" })).toBeNull();
  });

  it("opens the Create team dialog from the empty state", async () => {
    renderTable();
    await screen.findByText("No teams yet");
    const buttons = screen.getAllByRole("button", { name: "Create team" });
    fireEvent.click(buttons[buttons.length - 1] as HTMLElement);
    expect(await screen.findByRole("dialog", { name: "Create team" })).toBeTruthy();
  });

  it("shows the list error with Retry and loads on Retry", async () => {
    answer = () => Response.json({ status: 500, title: "x", detail: "x" }, { status: 500 });
    renderTable();
    expect(await screen.findByText("Couldn't load teams.")).toBeTruthy();
    answer = () => Response.json([teamRow()]);
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByRole("link", { name: "ops" })).toBeTruthy();
  });

  it("sorts by name ignoring case and pluralises grants", async () => {
    teams = [
      teamRow({ id: "t1", name: "beta", grantCount: 1 }),
      teamRow({ id: "t2", name: "Alpha", grantCount: 2 }),
      teamRow({ id: "t3", name: "charlie", grantCount: 0 }),
    ];
    renderTable();
    await screen.findByRole("link", { name: "Alpha" });
    const names = screen
      .getAllByRole("link")
      .map((l) => l.textContent)
      .filter((t) => ["Alpha", "beta", "charlie"].includes(t ?? ""));
    expect(names).toEqual(["Alpha", "beta", "charlie"]);
    expect(screen.getByText("1 grant")).toBeTruthy();
    expect(screen.getByText("2 grants")).toBeTruthy();
  });

  it("offers Open, Rename… and Delete team… in the row menu named for the team", async () => {
    teams = [teamRow()];
    renderTable();
    const trigger = await screen.findByRole("button", { name: "Actions for ops" });
    fireEvent.pointerDown(trigger, { button: 0, ctrlKey: false });
    fireEvent.click(trigger);
    expect(await screen.findByRole("menuitem", { name: "Open" })).toBeTruthy();
    expect(screen.getByRole("menuitem", { name: "Rename…" })).toBeTruthy();
    expect(screen.getByRole("menuitem", { name: "Delete team…" })).toBeTruthy();
  });

  it("renames from the row menu and reads the list again", async () => {
    teams = [teamRow()];
    renderTable();
    const trigger = await screen.findByRole("button", { name: "Actions for ops" });
    fireEvent.pointerDown(trigger, { button: 0, ctrlKey: false });
    fireEvent.click(trigger);
    fireEvent.click(await screen.findByRole("menuitem", { name: "Rename…" }));
    const dialog = await screen.findByRole("dialog", { name: "Rename team" });
    const name = within(dialog).getByLabelText("Team name") as HTMLInputElement;
    expect(name.value).toBe("ops");
    fireEvent.change(name, { target: { value: "platform" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Save changes" }));
    expect(await screen.findByText("Team renamed.")).toBeTruthy();
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(sent.find((s) => s.method === "PATCH")?.body).toEqual({
      name: "platform",
      updatedAt: "2026-10-07T10:00:00.123456789Z",
    });
  });

  it("deletes from the row menu only after the impact list has loaded", async () => {
    teams = [teamRow()];
    preview = () =>
      Response.json({
        impact: [
          {
            userId: "u1",
            username: "alice",
            environmentId: "e1",
            environmentName: "Production",
            before: "admin",
            after: "none",
          },
        ],
      });
    renderTable();
    const trigger = await screen.findByRole("button", { name: "Actions for ops" });
    fireEvent.pointerDown(trigger, { button: 0, ctrlKey: false });
    fireEvent.click(trigger);
    fireEvent.click(await screen.findByRole("menuitem", { name: "Delete team…" }));
    const dialog = await screen.findByRole("alertdialog", { name: "Delete team ops?" });
    expect(await within(dialog).findByText("Admin → No access")).toBeTruthy();
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete team" }));
    expect(await screen.findByText("Team ops deleted.")).toBeTruthy();
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(sent.some((s) => s.method === "DELETE" && s.path === "/api/teams/t1")).toBe(true);
  });
});
