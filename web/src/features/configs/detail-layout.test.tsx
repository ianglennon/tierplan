// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

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

// The config's page (plan 06-25, RES-08, UI-SPEC "Secrets and configs"): the frame, the Overview
// with its Details, Labels and Content cards, the Used by tab, Inspect, delete without a typed
// name, and the gone state.

type ConfigDetail = components["schemas"]["ConfigDetailBody"];

const ENV = "e1";
const ID = "cid-app";
const PAGE = `/env/${ENV}/configs/${ID}`;
const BASE = `/api/env/${ENV}`;
const YAML_TEXT = "name: api\nreplicas: 2\n";

function problem(status: number, type: string) {
  return Response.json({ type, title: "x", status }, { status });
}

const detail = (extra: Partial<ConfigDetail> = {}): ConfigDetail => ({
  id: ID,
  name: "app.yaml",
  labels: { team: "web", env: "prod" },
  owner: "",
  ownerKind: "",
  bayplanStackId: "",
  templating: "none",
  size: YAML_TEXT.length,
  data: btoa(YAML_TEXT),
  text: YAML_TEXT,
  createdAt: "2026-10-01T10:00:00Z",
  updatedAt: "2026-10-02T10:00:00Z",
  usedBy: 0,
  usedByRows: [],
  ...extra,
});

const inUse = (): ConfigDetail =>
  detail({
    usedBy: 2,
    usedByRows: [
      {
        service: "api",
        serviceId: "svc-api",
        stack: "shop",
        file: "/etc/app.yaml",
        uid: "0",
        gid: "0",
        mode: "0444",
      },
      {
        service: "worker",
        serviceId: "svc-worker",
        stack: "",
        file: "/app.yaml",
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
        inspect: { ID, Version: { Index: 7 }, Spec: { Name: "app.yaml", Labels: {} } },
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
        case `${BASE}/configs/${ID}`:
          return state.get();
        case `${BASE}/configs/${ID}/inspect`:
          return state.inspect();
        case `${BASE}/configs`:
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
  it("shows the breadcrumb Configs / name, the name and 'Not in use'", async () => {
    renderAt();
    expect(await screen.findByRole("heading", { name: "app.yaml" })).toBeTruthy();
    const crumb = screen.getByRole("navigation", { name: "Breadcrumb" });
    expect(within(crumb).getByRole("link", { name: "Configs" }).getAttribute("href")).toBe(
      `/env/${ENV}/configs`,
    );
    expect(crumb.textContent).toContain("app.yaml");
    expect(within(headerOf()).getByText("Not in use")).toBeTruthy();
    expect(document.title).toBe("app.yaml · Lab");
  });

  it("says how many services use it", async () => {
    state.get = () => Response.json(inUse());
    renderAt();
    await screen.findByRole("heading", { name: "app.yaml" });
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

  it("shows the tabs at once, with a skeleton for the content, before the config has loaded", async () => {
    state.get = () => new Promise<Response>(() => {});
    renderAt();
    expect(await screen.findByRole("tab", { name: "Overview" })).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "app.yaml" })).toBeNull();
    await screen.findByText("Loading the config");
    expect(screen.queryByRole("textbox")).toBeNull();
  });

  it("says the config is gone, with the way back, when the swarm no longer has it", async () => {
    state.get = () => problem(404, "config_not_found");
    renderAt();
    expect(await screen.findByText("This config is gone")).toBeTruthy();
    expect(
      screen.getByText(
        "It was removed, or a new version replaced it. Go back to the list to find it.",
      ),
    ).toBeTruthy();
    expect(screen.getByRole("link", { name: "Back to configs" }).getAttribute("href")).toBe(
      `/env/${ENV}/configs`,
    );
    expect(screen.queryByRole("tab", { name: "Overview" })).toBeNull();
  });

  it("shows a load error with a Retry that reads the config again", async () => {
    state.get = () => problem(503, "no_manager_connected");
    renderAt();
    await screen.findByText(/Couldn't load cid-app/);
    const before = state.sent.filter((s) => s === `GET ${BASE}/configs/${ID}`).length;
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() =>
      expect(state.sent.filter((s) => s === `GET ${BASE}/configs/${ID}`).length).toBeGreaterThan(
        before,
      ),
    );
  });
});

describe("the Overview tab", () => {
  it("shows the Details card with the ID, name, times, owner, templating and size", async () => {
    renderAt();
    const table = await screen.findByRole("table", { name: "Details" });
    const keys = within(table)
      .getAllByRole("row")
      .map((r) => r.querySelector("td")?.textContent);
    expect(keys).toEqual(["ID", "Name", "Created", "Updated", "Owner", "Templating", "Size"]);
    const owner = within(table).getByText("Owner").closest("tr") as HTMLElement;
    expect(owner.textContent).toContain("—");
    const templating = within(table).getByText("Templating").closest("tr") as HTMLElement;
    expect(templating.textContent).toContain("None");
    const size = within(table).getByText("Size").closest("tr") as HTMLElement;
    expect(size.textContent).toContain("22 B");
  });

  it("says Go template for a templated config, and Stack {name} for the owner", async () => {
    state.get = () =>
      Response.json(detail({ templating: "golang", owner: "shop", ownerKind: "stack" }));
    renderAt();
    await screen.findByText("Go template");
    expect(screen.getByText("Stack shop")).toBeTruthy();
  });

  it("shows the content in the viewer, coloured as YAML for a .yaml name", async () => {
    renderAt();
    const box = await screen.findByRole("textbox", { name: "Content of app.yaml, read only" });
    expect(box.textContent).toContain("replicas: 2");
    expect(screen.getByTestId("spec-viewer").getAttribute("data-language")).toBe("yaml");
  });

  it("offers Download only for content that is not text", async () => {
    state.get = () =>
      Response.json(detail({ name: "blob.bin", text: null, data: btoa("ÿþ\u0000") }));
    renderAt();
    await screen.findByText(
      "This config isn't text, so it can't be shown here. Download it to look at it.",
    );
    expect(screen.getByRole("button", { name: "Download" })).toBeTruthy();
    expect(screen.queryByRole("textbox")).toBeNull();
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
  it("opens a confirm with no typed name, deletes and goes back to the list", async () => {
    renderAt();
    const button = await screen.findByRole("button", { name: "Delete config app.yaml" });
    fireEvent.click(button);
    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).queryByLabelText(/to confirm/)).toBeNull();
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete config" }));
    await waitFor(() => expect(state.deleted).toHaveLength(1));
    expect(state.deleted[0]).toEqual({ path: `${BASE}/configs/${ID}`, search: "" });
    await waitFor(() => expect(screen.queryByRole("heading", { name: "app.yaml" })).toBeNull());
    await screen.findByRole("heading", { level: 1, name: "Configs" });
  });

  it("disables Delete while a service uses the config, with the reason and the in-use callout", async () => {
    state.get = () => Response.json(inUse());
    renderAt();
    const button = await screen.findByRole("button", { name: "Delete config app.yaml" });
    await waitFor(() => expect(button.getAttribute("aria-disabled")).toBe("true"));
    expect(document.body.textContent).toContain("In use by 2 services.");
    fireEvent.click(button);
    expect(screen.queryByRole("alertdialog")).toBeNull();
    const callout = screen.getByText("app.yaml is in use").closest("[role='alert']") as HTMLElement;
    expect(
      within(callout).getByText(
        "Services that mention it count, even with no running tasks. Remove it from them first.",
      ),
    ).toBeTruthy();
    expect(within(callout).getByText(/api/)).toBeTruthy();
    expect(state.deleted).toHaveLength(0);
  });

  it("shows a Viewer Delete disabled with Needs Admin, and still the content", async () => {
    state.me = makeMe({ envs: { [ENV]: "viewer" } });
    renderAt();
    const button = await screen.findByRole("button", { name: "Delete config app.yaml" });
    await waitFor(() => expect(button.getAttribute("aria-disabled")).toBe("true"));
    expect(document.body.textContent).toContain("Needs Admin");
    await screen.findByRole("textbox", { name: "Content of app.yaml, read only" });
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
    expect(within(rows[0] as HTMLElement).getByRole("link", { name: "api" })).toBeTruthy();
    expect(within(rows[1] as HTMLElement).getByText("1000 / 1000")).toBeTruthy();
  });

  it("says nothing uses the config", async () => {
    renderAt(`${PAGE}/used-by`);
    await screen.findByText("Nothing uses this config.");
  });
});

describe("the Inspect tab", () => {
  it("shows the manager's document", async () => {
    renderAt(`${PAGE}/inspect`);
    await screen.findByRole("tab", { name: "Inspect" });
    await waitFor(() => expect(document.body.textContent).toContain('"Index": 7'));
  });

  it("says the document could not be loaded, with a Retry", async () => {
    state.inspect = () => problem(503, "no_manager_connected");
    renderAt(`${PAGE}/inspect`);
    await screen.findByText(/Couldn't load the inspect output of app.yaml/);
    expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
  });
});

describe("the list route", () => {
  it("opens the create sheet for new=1 and drops it from the address when the sheet closes", async () => {
    const router = renderAt(`/env/${ENV}/configs?new=1`);
    expect(await screen.findByText("Create a config")).toBeTruthy();
    expect(router.state.location.search).toEqual({ new: 1 });
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByText("Create a config")).toBeNull());
    expect(router.state.location.search).toEqual({});
  });

  it("puts new=1 in the address when Create config is pressed", async () => {
    const router = renderAt(`/env/${ENV}/configs`);
    const buttons = await screen.findAllByRole("button", { name: "Create config" });
    fireEvent.click(buttons[0] as HTMLElement);
    await screen.findByText("Create a config");
    expect(router.state.location.search).toEqual({ new: 1 });
  });
});

describe("New version…", () => {
  it("is in the header beside Delete…, and opens the two-step rotate dialog", async () => {
    renderAt();
    const button = await screen.findByRole("button", { name: "New version of app.yaml" });
    expect(within(headerOf()).getByRole("button", { name: "Delete config app.yaml" })).toBeTruthy();
    fireEvent.click(button);
    expect(await screen.findByRole("heading", { name: "New version of app.yaml" })).toBeTruthy();
    expect(screen.getByText("Step 1 of 2")).toBeTruthy();
  });

  it("shows a Viewer New version… disabled with Needs Admin", async () => {
    state.me = makeMe({ envs: { [ENV]: "viewer" } });
    renderAt();
    const button = await screen.findByRole("button", { name: "New version of app.yaml" });
    await waitFor(() => expect(button.getAttribute("aria-disabled")).toBe("true"));
    expect(document.body.textContent).toContain("Needs Admin");
    fireEvent.click(button);
    expect(screen.queryByRole("dialog")).toBeNull();
  });
});
