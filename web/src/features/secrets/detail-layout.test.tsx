// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { detailRetry } from "@/api/container-detail";
import { queryClient } from "@/api/query-client";
import type { components } from "@/api/schema";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { routeTree } from "@/routeTree.gen";
import "@/i18n";
import { makeMe } from "@/test-me";

// The secret's page (plan 06-23, RES-08, UI-SPEC "Secrets and configs"): the frame, the metadata
// Overview with the write-only Value card, the Used by tab, Inspect, delete, and the gone state.

type SecretDetail = components["schemas"]["SecretDetailBody"];

const ENV = "e1";
const ID = "sid-db-password";
const PAGE = `/env/${ENV}/secrets/${ID}`;
const BASE = `/api/env/${ENV}`;

function problem(status: number, type: string) {
  return Response.json({ type, title: "x", status }, { status });
}

const detail = (extra: Partial<SecretDetail> = {}): SecretDetail => ({
  id: ID,
  name: "db-password",
  labels: { team: "data", env: "prod" },
  owner: "",
  ownerKind: "",
  coamingStackId: "",
  createdAt: "2026-10-01T10:00:00Z",
  updatedAt: "2026-10-02T10:00:00Z",
  usedBy: 0,
  usedByRows: [],
  ...extra,
});

const inUse = (): SecretDetail =>
  detail({
    usedBy: 2,
    usedByRows: [
      {
        service: "api",
        serviceId: "svc-api",
        stack: "shop",
        file: "db_password",
        uid: "0",
        gid: "0",
        mode: "0444",
      },
      {
        service: "worker",
        serviceId: "svc-worker",
        stack: "",
        file: "/run/secrets/pw",
        uid: "1000",
        gid: "1000",
        mode: "0400",
      },
    ],
  });

interface State {
  me: ReturnType<typeof makeMe>;
  sent: string[];
  get: () => Response | Promise<Response>;
  inspect: () => Response | Promise<Response>;
  deleted: { path: string; search: string }[];
  remove: () => Response | Promise<Response>;
}

let state: State;
const defaults = queryClient.getDefaultOptions();

beforeEach(() => {
  state = {
    me: makeMe({ envs: { [ENV]: "admin" } }),
    sent: [],
    get: () => Response.json(detail()),
    inspect: () =>
      Response.json({
        inspect: { ID, Version: { Index: 7 }, Spec: { Name: "db-password", Labels: {} } },
      }),
    deleted: [],
    remove: () => new Response(null, { status: 204 }),
  };
  queryClient.setDefaultOptions({ queries: { retry: false, refetchOnWindowFocus: false } });
  detailRetry.attempts = 0;
  setCsrfToken(null);
  FakeEventSource.reset();
  vi.stubGlobal("EventSource", FakeEventSource);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request): Promise<Response> => {
      const url = new URL(req.url);
      state.sent.push(`${req.method} ${url.pathname}`);
      if (req.method === "DELETE") {
        state.deleted.push({ path: url.pathname, search: url.search });
        return state.remove();
      }
      switch (url.pathname) {
        case "/api/instance":
          return Response.json({ instanceName: "Lab", needsSetup: false, rememberDays: 30 });
        case "/api/me":
          return Response.json(state.me);
        case "/api/version":
          return Response.json({
            commit: "",
            display: "v0.1.0",
            sourceTreeUrl: "https://example.test/tree",
            sourceUrl: "https://example.test",
            version: "v0.1.0",
          });
        case "/api/environments":
          return Response.json({
            items: [
              {
                id: ENV,
                name: "Lab swarm",
                kind: "local",
                type: "swarm-manager",
                envType: "swarm",
                enabled: true,
                isSelfHost: true,
                createdAt: "2026-10-01T10:00:00Z",
                status: { state: "online" },
              },
            ],
            total: 1,
          });
        case `${BASE}/summary`:
          return Response.json({
            environment: {
              id: ENV,
              name: "Lab swarm",
              type: "swarm-manager",
              status: { state: "online" },
            },
            engine: { name: "Docker Engine", version: "29.1.0", apiVersion: "1.52" },
            counts: {
              containers: 1,
              containersRunning: 1,
              containersStopped: 0,
              images: 0,
              volumes: 0,
              networks: 0,
            },
            isWorker: false,
            fetchedAt: "2026-10-01T10:00:00Z",
          });
        case `${BASE}/secrets/${ID}`:
          return state.get();
        case `${BASE}/secrets/${ID}/inspect`:
          return state.inspect();
        case `${BASE}/secrets`:
          return Response.json({ items: [], total: 0, unfilteredTotal: 0, capped: false });
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  queryClient.clear();
  queryClient.setDefaultOptions(defaults);
  detailRetry.attempts = 1;
  document.title = "";
});

function renderAt(path = PAGE) {
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

function headerOf(): HTMLElement {
  const header = screen.getByRole("heading", { level: 2 }).closest("header");
  if (!header) throw new Error("no header");
  return header;
}

describe("the page frame", () => {
  it("shows the breadcrumb Secrets / name, the name and 'Not in use'", async () => {
    renderAt();
    expect(await screen.findByRole("heading", { name: "db-password" })).toBeTruthy();
    const crumb = screen.getByRole("navigation", { name: "Breadcrumb" });
    expect(within(crumb).getByRole("link", { name: "Secrets" }).getAttribute("href")).toBe(
      `/env/${ENV}/secrets`,
    );
    expect(crumb.textContent).toContain("db-password");
    expect(within(headerOf()).getByText("Not in use")).toBeTruthy();
    expect(document.title).toBe("db-password · Lab");
  });

  it("says how many services use it", async () => {
    state.get = () => Response.json(inUse());
    renderAt();
    await screen.findByRole("heading", { name: "db-password" });
    expect(within(headerOf()).getByText("Used by 2 services")).toBeTruthy();
  });

  it("has the tabs Overview, Used by and Inspect, the first at the page's own address", async () => {
    renderAt();
    const overview = await screen.findByRole("tab", { name: "Overview" });
    expect(overview.getAttribute("href")).toBe(PAGE);
    expect(screen.getAllByRole("tab").map((tab) => tab.textContent)).toEqual([
      "Overview",
      "Used by",
      "Inspect",
    ]);
    expect(screen.getByRole("tab", { name: "Used by" }).getAttribute("href")).toBe(
      `${PAGE}/used-by`,
    );
    expect(screen.getByRole("tab", { name: "Inspect" }).getAttribute("href")).toBe(
      `${PAGE}/inspect`,
    );
  });

  it("shows the tabs at once, before the secret has loaded", async () => {
    state.get = () => new Promise<Response>(() => {});
    renderAt();
    expect(await screen.findByRole("tab", { name: "Overview" })).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "db-password" })).toBeNull();
  });

  it("says the secret is gone, with the way back, when the swarm no longer has it", async () => {
    state.get = () => problem(404, "secret_not_found");
    renderAt();
    expect(await screen.findByText("This secret is gone")).toBeTruthy();
    expect(
      screen.getByText(
        "It was removed, or a new version replaced it. Go back to the list to find it.",
      ),
    ).toBeTruthy();
    expect(screen.getByRole("link", { name: "Back to secrets" }).getAttribute("href")).toBe(
      `/env/${ENV}/secrets`,
    );
    expect(screen.queryByRole("tab", { name: "Overview" })).toBeNull();
  });

  it("shows a load error with a Retry that reads the secret again", async () => {
    state.get = () => problem(503, "no_manager_connected");
    renderAt();
    await screen.findByText(/Couldn't load sid-db-password/);
    const before = state.sent.filter((s) => s === `GET ${BASE}/secrets/${ID}`).length;
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() =>
      expect(state.sent.filter((s) => s === `GET ${BASE}/secrets/${ID}`).length).toBeGreaterThan(
        before,
      ),
    );
  });
});

describe("the Overview tab", () => {
  it("shows the Details card with the ID, name, times and owner, and a dash for no owner", async () => {
    renderAt();
    const table = await screen.findByRole("table", { name: "Details" });
    const keys = within(table)
      .getAllByRole("row")
      .map((r) => r.querySelector("td")?.textContent);
    expect(keys).toEqual(["ID", "Name", "Created", "Updated", "Owner"]);
    expect(within(table).getByText(ID)).toBeTruthy();
    expect(within(table).getByText("db-password")).toBeTruthy();
    const owner = within(table).getByText("Owner").closest("tr") as HTMLElement;
    expect(owner.textContent).toContain("—");
  });

  it("shows 'Stack shop' for the owner", async () => {
    state.get = () => Response.json(detail({ owner: "shop", ownerKind: "stack" }));
    renderAt();
    await screen.findByText("Stack shop");
  });

  it("states that the value is write-only, and has no way to reveal it, even with the reveal permission", async () => {
    state.me = makeMe({ envs: { [ENV]: "admin" }, extras: { [ENV]: ["secrets.reveal"] } });
    renderAt();
    await screen.findByText("Value");
    expect(
      screen.getByText(
        "The value is write-only. Docker never returns a secret's value after it's created, and Coaming can't show it, even with the reveal secrets permission.",
      ),
    ).toBeTruthy();
    expect(screen.queryByRole("button", { name: /show value|hide value|reveal/i })).toBeNull();
  });

  it("lists the labels sorted by name, or says there are none", async () => {
    renderAt();
    await screen.findByText("Labels");
    const keys = screen.getAllByRole("row").map((r) => r.textContent);
    const env = keys.findIndex((k) => k?.startsWith("env"));
    const team = keys.findIndex((k) => k?.startsWith("team"));
    expect(env).toBeGreaterThan(-1);
    expect(team).toBeGreaterThan(env);
    cleanup();
    state.get = () => Response.json(detail({ labels: {} }));
    renderAt();
    await screen.findByText("No labels.");
  });
});

describe("delete", () => {
  it("opens the typed-name confirm, deletes with the name and goes back to the list", async () => {
    renderAt();
    const button = await screen.findByRole("button", { name: "Delete secret db-password" });
    fireEvent.click(button);
    const dialog = await screen.findByRole("alertdialog");
    fireEvent.change(within(dialog).getByLabelText("Type db-password to confirm"), {
      target: { value: "db-password" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete secret" }));
    await waitFor(() => expect(state.deleted).toHaveLength(1));
    expect(state.deleted[0]).toEqual({
      path: `${BASE}/secrets/${ID}`,
      search: "?confirm=db-password",
    });
    await waitFor(() => expect(screen.queryByRole("heading", { name: "db-password" })).toBeNull());
    await screen.findByRole("heading", { level: 1, name: "Secrets" });
  });

  it("disables Delete while a service uses the secret, with the reason and the in-use callout", async () => {
    state.get = () => Response.json(inUse());
    renderAt();
    const button = await screen.findByRole("button", { name: "Delete secret db-password" });
    await waitFor(() => expect(button.getAttribute("aria-disabled")).toBe("true"));
    expect(document.body.textContent).toContain("In use by 2 services.");
    fireEvent.click(button);
    expect(screen.queryByRole("alertdialog")).toBeNull();
    const callout = screen
      .getByText("db-password is in use")
      .closest("[role='alert']") as HTMLElement;
    expect(
      within(callout).getByText(
        "Services that mention it count, even with no running tasks. Remove it from them first.",
      ),
    ).toBeTruthy();
    expect(within(callout).getByText(/api/)).toBeTruthy();
    expect(within(callout).getByText(/worker/)).toBeTruthy();
    expect(state.deleted).toHaveLength(0);
  });

  it("shows a Viewer Delete disabled with Needs Admin", async () => {
    state.me = makeMe({ envs: { [ENV]: "viewer" } });
    renderAt();
    const button = await screen.findByRole("button", { name: "Delete secret db-password" });
    await waitFor(() => expect(button.getAttribute("aria-disabled")).toBe("true"));
    expect(document.body.textContent).toContain("Needs Admin");
  });
});

describe("the Used by tab", () => {
  it("lists each service with its stack, file, UID / GID and mode, as links to the service", async () => {
    state.get = () => Response.json(inUse());
    renderAt(`${PAGE}/used-by`);
    const table = await screen.findByRole("table", { name: "Used by" });
    expect(
      within(table)
        .getAllByRole("columnheader")
        .map((h) => h.textContent),
    ).toEqual(["Service", "Stack", "File", "UID / GID", "Mode"]);
    const rows = within(table).getAllByRole("row").slice(1);
    expect(rows.map((r) => r.querySelector("td")?.textContent)).toEqual(["api", "worker"]);
    const api = rows[0] as HTMLElement;
    expect(within(api).getByRole("link", { name: "api" }).getAttribute("href")).toBe(
      `/env/${ENV}/services/svc-api`,
    );
    expect(within(api).getByText("shop")).toBeTruthy();
    expect(within(api).getByText("db_password")).toBeTruthy();
    expect(within(api).getByText("0 / 0")).toBeTruthy();
    expect(within(api).getByText("0444")).toBeTruthy();
    expect(within(rows[1] as HTMLElement).getByText("1000 / 1000")).toBeTruthy();
  });

  it("says nothing uses the secret", async () => {
    renderAt(`${PAGE}/used-by`);
    await screen.findByText("Nothing uses this secret.");
  });
});

describe("the Inspect tab", () => {
  it("shows the manager's document", async () => {
    renderAt(`${PAGE}/inspect`);
    await screen.findByRole("tab", { name: "Inspect" });
    await waitFor(() => expect(document.body.textContent).toContain('"Index": 7'));
    expect(document.body.textContent).not.toContain('"Data"');
  });

  it("says the document could not be loaded, with a Retry", async () => {
    state.inspect = () => problem(503, "no_manager_connected");
    renderAt(`${PAGE}/inspect`);
    await screen.findByText(/Couldn't load the inspect output of db-password/);
    expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
  });
});

describe("the list route", () => {
  it("opens the create sheet for new=1 and drops it from the address when the sheet closes", async () => {
    const router = renderAt(`/env/${ENV}/secrets?new=1`);
    expect(await screen.findByText("Create a secret")).toBeTruthy();
    expect(router.state.location.search).toEqual({ new: 1 });
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByText("Create a secret")).toBeNull());
    expect(router.state.location.search).toEqual({});
  });

  it("puts new=1 in the address when Create secret is pressed", async () => {
    const router = renderAt(`/env/${ENV}/secrets`);
    const buttons = await screen.findAllByRole("button", { name: "Create secret" });
    fireEvent.click(buttons[0] as HTMLElement);
    await screen.findByText("Create a secret");
    expect(router.state.location.search).toEqual({ new: 1 });
  });
});

describe("New version…", () => {
  it("is in the header beside Delete…, and opens the two-step rotate dialog", async () => {
    renderAt();
    const button = await screen.findByRole("button", { name: "New version of db-password" });
    expect(
      within(headerOf()).getByRole("button", { name: "Delete secret db-password" }),
    ).toBeTruthy();
    fireEvent.click(button);
    expect(await screen.findByRole("heading", { name: "New version of db-password" })).toBeTruthy();
    expect(screen.getByText("Step 1 of 2")).toBeTruthy();
  });

  it("shows a Viewer New version… disabled with Needs Admin", async () => {
    state.me = makeMe({ envs: { [ENV]: "viewer" } });
    renderAt();
    const button = await screen.findByRole("button", { name: "New version of db-password" });
    await waitFor(() => expect(button.getAttribute("aria-disabled")).toBe("true"));
    expect(document.body.textContent).toContain("Needs Admin");
    fireEvent.click(button);
    expect(screen.queryByRole("dialog")).toBeNull();
  });
});
