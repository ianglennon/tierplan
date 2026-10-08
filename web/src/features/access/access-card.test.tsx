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

const defaultQueryOptions = queryClient.getDefaultOptions();

beforeEach(() => {
  queryClient.setDefaultOptions({ queries: { retry: false, refetchOnWindowFocus: false } });
  access = { instanceAdmin: false, environments: [], grants: [] };
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
      if (path === "/api/me") return Response.json(makeMe({ instanceAdmin: true, id: "me" }));
      if (path === "/api/version") {
        return Response.json({
          commit: "",
          display: "v0.1.0",
          sourceTreeUrl: "https://example.test/tree",
          sourceUrl: "https://example.test",
          version: "v0.1.0",
        });
      }
      if (path === "/api/users/u1") return Response.json(userBody());
      if (path === "/api/users/u1/sessions") return Response.json([]);
      if (path === "/api/users/u1/access") return Response.json(access);
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
