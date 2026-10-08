// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

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
  setCsrfToken(null);
  FakeEventSource.reset();
  vi.stubGlobal("EventSource", FakeEventSource);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
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
      if (path === "/api/env/e1/access/candidates") return Response.json(candidates);
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
    fireEvent.click(within(dialog).getByRole("combobox", { name: "Who" }));
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
    fireEvent.click(within(dialog).getByRole("combobox", { name: "Who" }));
    fireEvent.click(await screen.findByRole("option", { name: /erin/ }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Add access" }));
    expect(await within(dialog).findByText("You can't give that access.")).toBeTruthy();
    expect(screen.getByRole("dialog", { name: "Add access" })).toBeTruthy();
  });
});
