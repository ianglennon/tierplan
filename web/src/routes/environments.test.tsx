// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { EnvironmentItem } from "@/api/environments";
import { queryClient } from "@/api/query-client";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { routeTree } from "@/routeTree.gen";
import "@/i18n";
import { makeMe } from "@/test-me";
import { splitPermissions } from "@/test-permissions";

function permissions(...perms: string[]) {
  return splitPermissions(perms);
}

const ADMIN = makeMe({
  instanceAdmin: true,
  permissions: permissions("environments.read", "environments.create", "environments.token"),
});

function env(id: string, name: string, state: EnvironmentItem["status"]["state"], extra = {}) {
  return {
    id,
    name,
    kind: "agent",
    enabled: true,
    isSelfHost: false,
    isLocalSocket: false,
    createdAt: "2026-10-01T10:00:00Z",
    engine: {},
    status: { state },
    agentVersion: null,
    agents: null,
    engineVersion: null,
    failingServices: null,
    lastSeen: null,
    nodes: null,
    updateAvailable: false,
    ...extra,
  };
}

const ONLINE = env("e1", "nas", "online", {
  envType: "standalone",
  nodes: { ready: 1, total: 1 },
  engineVersion: "28.5.1",
});
const OFFLINE = env("e2", "edge", "offline", {
  envType: "standalone",
  status: { state: "offline", reason: "agent_offline", since: "2026-10-03T09:00:00Z" },
});

let me: Record<string, unknown>;
let list: () => Response | Promise<Response>;
let sent: string[];
let fleetSummary: () => Response | Promise<Response>;
let fleetFailing: (query: URLSearchParams) => Response | Promise<Response>;
let fleetUpdates: (query: URLSearchParams) => Response | Promise<Response>;

// The Overview's tiles and tabs read the fleet; most tests here are about the table, so by default
// the fleet is whole and nothing is failing.
const SUMMARY = {
  environments: { total: 2, needAttention: 1 },
  failing: { count: 0, environments: 0 },
  updates: { count: 0, checksFailed: 0, allChecked: true, lastCheckedAt: "" },
  pools: { nearFull: 0, almostFull: 0 },
  notIncluded: [],
  readAt: "2026-10-03T10:00:00Z",
};

// The Updates tab reads its own list; a test that opens it gets a fleet with nothing to update.
const EMPTY_UPDATES = {
  counts: { updates: 0, problems: 0, ignored: 0, all: 0 },
  coverage: {
    checkable: 0,
    checked: 0,
    environments: 0,
    failed: 0,
    neverChecked: 0,
    skipped: 0,
    firstCheckAt: "",
    lastCheckedAt: "",
  },
  groups: [],
  notIncluded: [],
  readAt: SUMMARY.readAt,
};

beforeEach(() => {
  me = ADMIN;
  sent = [];
  list = () => Response.json({ items: [ONLINE, OFFLINE] });
  fleetSummary = () => Response.json(SUMMARY);
  fleetFailing = () => Response.json({ items: [], notIncluded: [], readAt: SUMMARY.readAt });
  fleetUpdates = () => Response.json(EMPTY_UPDATES);
  setCsrfToken(null);
  FakeEventSource.reset();
  vi.stubGlobal("EventSource", FakeEventSource);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      sent.push(`${req.method} ${path}`);
      if (path === "/api/instance") {
        return Response.json({ instanceName: "Lab", needsSetup: false, rememberDays: 30 });
      }
      if (path === "/api/me") return Response.json(me);
      if (path === "/api/version") {
        return Response.json({
          commit: "",
          display: "v0.1.0",
          sourceTreeUrl: "https://example.test/tree",
          sourceUrl: "https://example.test",
          version: "v0.1.0",
        });
      }
      if (path === "/api/local-host") {
        return Response.json({
          socketPath: "/var/run/docker.sock",
          detected: false,
          managed: false,
        });
      }
      if (path === "/api/environments") return list();
      if (path === "/api/fleet/summary") return fleetSummary();
      if (path === "/api/fleet/failing") return fleetFailing(new URL(req.url).searchParams);
      if (path === "/api/fleet/updates") return fleetUpdates(new URL(req.url).searchParams);
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  queryClient.clear();
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

// The sidebar's item, not the Overview's tile of the same name.
const menuLink = (name: string) => {
  const found = screen
    .getAllByRole("link", { name })
    .find((a) => a.getAttribute("data-sidebar") === "menu-button");
  if (!found) throw new Error(`no sidebar item named ${name}`);
  return found;
};

const names = () =>
  within(screen.getByRole("table"))
    .getAllByRole("link")
    .map((a) => a.textContent);
const stream = () => {
  const s = FakeEventSource.instances.find((i) => i.url === "/api/environments/stream");
  if (!s) throw new Error("the health stream is not open");
  return s;
};

describe("/environments", () => {
  it("shows the title, the count and the environments, problems first", async () => {
    renderAt("/environments");
    expect(await screen.findByRole("heading", { level: 1, name: "Environments" })).toBeTruthy();
    await screen.findByRole("table");
    expect(screen.getByText("2 environments")).toBeTruthy();
    expect(names()).toEqual(["edge", "nas"]);
    expect(document.title).toBe("Environments · Lab");
  });

  it("reads one environment as '1 environment'", async () => {
    list = () => Response.json({ items: [ONLINE] });
    renderAt("/environments");
    expect(await screen.findByText("1 environment")).toBeTruthy();
  });

  it("writes the sort to the URL: ascending first, then descending", async () => {
    const router = renderAt("/environments");
    await screen.findByRole("table");
    fireEvent.click(screen.getByRole("button", { name: "Sort by Name" }));
    await waitFor(() =>
      expect(router.state.location.search).toMatchObject({ sort: "name", dir: "asc" }),
    );
    expect(router.state.location.searchStr).toContain("sort=name");
    expect(router.state.location.searchStr).toContain("dir=asc");
    expect(names()).toEqual(["edge", "nas"]);
    fireEvent.click(screen.getByRole("button", { name: "Sort by Name" }));
    await waitFor(() => expect(router.state.location.search).toMatchObject({ dir: "desc" }));
    expect(names()).toEqual(["nas", "edge"]);
  });

  it("reads the sort from the URL", async () => {
    renderAt("/environments?sort=name&dir=desc");
    await screen.findByRole("link", { name: "nas" });
    expect(names()).toEqual(["nas", "edge"]);
  });

  it("ignores a sort it does not know and shows problems first", async () => {
    renderAt("/environments?sort=bogus&dir=sideways");
    await screen.findByRole("link", { name: "nas" });
    expect(names()).toEqual(["edge", "nas"]);
  });

  it("shows six skeleton rows while loading, and never the empty state", async () => {
    list = () => new Promise<Response>(() => {});
    renderAt("/environments");
    const table = await screen.findByRole("table");
    expect(table.getAttribute("aria-busy")).toBe("true");
    expect(table.querySelectorAll("tbody tr")).toHaveLength(6);
    expect(screen.queryByText("No environments yet")).toBeNull();
  });

  it("shows the empty state with Add environment and the local host opt-in for someone who can create", async () => {
    list = () => Response.json({ items: [] });
    renderAt("/environments");
    expect(await screen.findByText("No environments yet")).toBeTruthy();
    expect(
      screen.getByText(
        "Add a swarm or a standalone Docker host. TierPlan gives you one command that installs a small agent on it. The host needs no open inbound port.",
      ),
    ).toBeTruthy();
    // One primary button per view: the header's gives way to the empty state's.
    expect(screen.getAllByRole("button", { name: "Add environment" })).toHaveLength(1);
    expect(await screen.findByRole("heading", { name: "No Docker socket found" })).toBeTruthy();
  });

  it("tells someone who cannot create to ask an admin", async () => {
    me = { ...ADMIN, permissions: permissions("environments.read") };
    list = () => Response.json({ items: [] });
    renderAt("/environments");
    expect(await screen.findByText("Ask an admin to add an environment.")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Add environment" })).toBeNull();
  });

  it("shows Couldn't load environments with Retry, and Retry reads the list again", async () => {
    list = () => new Response("boom", { status: 500 });
    renderAt("/environments");
    // The client retries a failed read once before it gives up.
    expect(await screen.findByText("Couldn't load environments.")).toBeTruthy();
    expect(screen.getByText("Check the connection and try again.")).toBeTruthy();
    list = () => Response.json({ items: [ONLINE] });
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByRole("table")).toBeTruthy();
    expect(screen.queryByText("Couldn't load environments.")).toBeNull();
  });

  it("puts Add environment in the header for someone who can create, and leaves it out otherwise", async () => {
    renderAt("/environments");
    await screen.findByRole("table");
    expect(screen.getByRole("button", { name: "Add environment" })).toBeTruthy();
    cleanup();
    me = { ...ADMIN, permissions: permissions("environments.read") };
    queryClient.clear();
    renderAt("/environments");
    await screen.findByRole("table");
    expect(screen.queryByRole("button", { name: "Add environment" })).toBeNull();
  });

  it("shows No permission to someone who may read no environment", async () => {
    me = { ...ADMIN, permissions: { global: [], environments: {} } };
    renderAt("/environments");
    expect(
      await screen.findByRole("heading", { name: "You don't have permission to view this page" }),
    ).toBeTruthy();
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("shows the no-access state, not No permission, to someone with no access to any environment (D-08)", async () => {
    me = makeMe({ envs: {}, administrators: ["admin"] });
    renderAt("/environments");
    expect(
      await screen.findByRole("heading", { name: "You don't have access to any environments yet" }),
    ).toBeTruthy();
    expect(screen.getByText("Administrators: admin")).toBeTruthy();
    expect(screen.queryByText("You don't have permission to view this page")).toBeNull();
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("shows Live once the stream opens, Reconnecting… while it retries, and nothing once polling", async () => {
    renderAt("/environments");
    await screen.findByRole("table");
    expect(screen.queryByText("Live")).toBeNull();
    await act(async () => stream().open());
    expect(screen.getByText("Live")).toBeTruthy();
    await act(async () => stream().fail());
    expect(screen.getByText("Reconnecting…")).toBeTruthy();
    await act(async () => stream().fail(true));
    expect(screen.queryByText("Reconnecting…")).toBeNull();
    expect(screen.queryByText("Live")).toBeNull();
  });

  it("updates a row from the stream without reading the list again", async () => {
    renderAt("/environments");
    await screen.findByRole("link", { name: "nas" });
    await act(async () => stream().open());
    const reads = sent.filter((s) => s === "GET /api/environments").length;
    await act(async () => {
      stream().sendFrame(
        "env.status",
        {
          envId: "e1",
          at: "2026-10-03T10:00:00Z",
          environment: { ...ONLINE, status: { state: "degraded", reason: "clock_skew" } },
        },
        "3",
      );
    });
    // The cache notifies its observers on the next tick.
    await waitFor(() => {
      const row = screen
        .getAllByRole("row")
        .find((r) => within(r).queryByText("nas")) as HTMLElement;
      expect(within(row).getByText("Degraded")).toBeTruthy();
    });
    expect(sent.filter((s) => s === "GET /api/environments")).toHaveLength(reads);
  });
});

describe("the install dialog on /environments", () => {
  const PENDING = env("e3", "new host", "pending");

  it("Add environment opens the dialog on step 1 and Cancel closes it", async () => {
    renderAt("/environments");
    await screen.findByRole("table");
    fireEvent.click(screen.getByRole("button", { name: "Add environment" }));
    expect(await screen.findByRole("heading", { name: "Add environment", level: 2 })).toBeTruthy();
    expect(screen.getByLabelText("Name (optional)")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(sent.filter((s) => s.startsWith("POST"))).toEqual([]);
  });

  it("the empty state's Add environment opens it too", async () => {
    list = () => Response.json({ items: [] });
    renderAt("/environments");
    fireEvent.click(await screen.findByRole("button", { name: "Add environment" }));
    expect(await screen.findByLabelText("Name (optional)")).toBeTruthy();
  });

  it("puts focus back on the Add environment button when the dialog closes", async () => {
    renderAt("/environments");
    await screen.findByRole("table");
    const add = screen.getByRole("button", { name: "Add environment" });
    fireEvent.click(add);
    await screen.findByRole("heading", { name: "Add environment", level: 2 });
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    await waitFor(() => expect(document.activeElement).toBe(add));
  });

  it("puts focus back on the empty state's Add environment button", async () => {
    list = () => Response.json({ items: [] });
    renderAt("/environments");
    // The header button shows while the list loads, so wait for the empty state.
    await screen.findByText("No environments yet");
    const add = screen.getByRole("button", { name: "Add environment" });
    fireEvent.click(add);
    await screen.findByLabelText("Name (optional)");
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    await waitFor(() => expect(document.activeElement).toBe(add));
  });

  it("puts focus back on the row's Finish setup button", async () => {
    list = () => Response.json({ items: [ONLINE, PENDING] });
    renderAt("/environments");
    const finish = await screen.findByRole("button", { name: "Finish setup" });
    fireEvent.click(finish);
    await screen.findByRole("heading", { name: "Show install command", level: 2 });
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    await waitFor(() => expect(document.activeElement).toBe(finish));
  });

  it("Finish setup opens the rotate confirm for that environment and posts nothing until confirmed", async () => {
    list = () => Response.json({ items: [ONLINE, PENDING] });
    renderAt("/environments");
    fireEvent.click(await screen.findByRole("button", { name: "Finish setup" }));
    expect(
      await screen.findByRole("heading", { name: "Show install command", level: 2 }),
    ).toBeTruthy();
    expect(screen.getByText(/This replaces the previous token\./)).toBeTruthy();
    expect(sent.filter((s) => s.startsWith("POST"))).toEqual([]);
  });
});

describe("the sidebar's Environments item", () => {
  it("sits above the environment's own sections and is active only on /environments", async () => {
    renderAt("/environments");
    await screen.findByRole("table");
    const link = menuLink("Environments");
    expect(link.getAttribute("aria-current")).toBe("page");
    cleanup();

    renderAt("/env/e1");
    const dashboard = await screen.findByRole("link", { name: "Dashboard" });
    const other = menuLink("Environments");
    expect(other.getAttribute("aria-current")).toBeNull();
    expect(
      other.compareDocumentPosition(dashboard) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
  });

  it("is left out for someone who may read no environment", async () => {
    me = { ...ADMIN, permissions: { global: [], environments: {} } };
    renderAt("/environments");
    await screen.findByRole("heading", { name: "You don't have permission to view this page" });
    expect(screen.queryByRole("link", { name: "Environments" })).toBeNull();
  });
});

describe("the fold notice (D-48, UI-SPEC Header bell additions: Fold offered)", () => {
  const SWARM = env("s1", "Prod swarm", "online", {
    envType: "swarm",
    foldOffered: true,
    nodes: { ready: 2, total: 2 },
  });
  const LOCAL = env("l1", "This host", "online", {
    kind: "local",
    isLocalSocket: true,
    foldOffered: true,
    envType: "swarm",
  });
  const MANAGER = {
    ...ADMIN,
    permissions: splitPermissions(["environments.read", "environments.manage"]),
  };

  it("tells a manager that this host is part of the swarm and links to the fold card", async () => {
    me = MANAGER;
    list = () => Response.json({ items: [SWARM, LOCAL] });
    renderAt("/environments");
    expect(await screen.findByText("This host is part of Prod swarm")).toBeTruthy();
    expect(
      screen.getByText("Fold it into the swarm environment so it isn't listed twice."),
    ).toBeTruthy();
    expect(screen.getByRole("link", { name: "Review" }).getAttribute("href")).toBe(
      "/settings/environments/s1#fold",
    );
  });

  it("says nothing to someone who may not manage environments", async () => {
    list = () => Response.json({ items: [SWARM, LOCAL] });
    renderAt("/environments");
    await screen.findByRole("table");
    expect(screen.queryByText(/This host is part of/)).toBeNull();
  });

  it("says nothing when no fold is offered", async () => {
    me = MANAGER;
    renderAt("/environments");
    await screen.findByRole("table");
    expect(screen.queryByText(/This host is part of/)).toBeNull();
  });
});

describe("the fleet tiles and tabs (D-17)", () => {
  const A = "0190a1b2-0000-7000-8000-00000000000a";
  const B = "0190a1b2-0000-7000-8000-00000000000b";
  const FA = env(A, "alpha", "online", { envType: "swarm", nodes: { ready: 1, total: 1 } });
  const FB = env(B, "bravo", "online", { envType: "swarm", nodes: { ready: 1, total: 1 } });

  const FAILING_ITEM = {
    kind: "service",
    id: "svc1",
    name: "web",
    envId: B,
    envName: "bravo",
    stack: "shop",
    state: "1 of 2 running",
    running: 1,
    desired: 2,
    since: "2026-10-03T09:00:00Z",
    lastError: "task failed",
    node: "",
    nodeId: "",
  };

  const search = (r: ReturnType<typeof renderAt>) =>
    r.state.location.search as Record<string, unknown>;
  const enc = (ids: string[]) => encodeURIComponent(JSON.stringify(ids));

  beforeEach(() => {
    list = () => Response.json({ items: [FA, FB] });
    fleetSummary = () =>
      Response.json({
        ...SUMMARY,
        environments: { total: 2, needAttention: 0 },
        failing: { count: 1, environments: 1 },
      });
    fleetFailing = () =>
      Response.json({ items: [FAILING_ITEM], notIncluded: [], readAt: SUMMARY.readAt });
  });

  it("shows the four tiles above the tab list, with the tabs named as the spec says", async () => {
    renderAt("/environments");
    const tablist = await screen.findByRole("tablist", { name: "Fleet views" });
    expect(
      within(tablist)
        .getAllByRole("tab")
        .map((t) => t.textContent),
    ).toEqual(["Environments", "Failing services", "Updates", "Address pools"]);
    const failing = await screen.findByRole("link", {
      name: /^Failing services ?1 ?in 1 environment$/,
    });
    expect(
      failing.compareDocumentPosition(tablist) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
    expect(
      await screen.findByRole("link", { name: /^Environments ?2 ?All reporting$/ }),
    ).toBeTruthy();
    expect(screen.getByRole("tab", { name: "Environments" }).getAttribute("aria-selected")).toBe(
      "true",
    );
  });

  it("opens a failing service of another environment from the Failing services tile", async () => {
    const router = renderAt("/environments");
    fireEvent.click(
      await screen.findByRole("link", { name: /^Failing services ?1 ?in 1 environment$/ }),
    );
    const table = await screen.findByRole("table", { name: /Failing services/ });
    await waitFor(() => expect(search(router).tab).toBe("failing"));
    expect(await within(table).findByText("1 of 2 running")).toBeTruthy();
    expect(within(table).getByRole("link", { name: "bravo" })).toBeTruthy();
    expect(within(table).getByRole("link", { name: "web" }).getAttribute("href")).toBe(
      `/env/${B}/services/svc1`,
    );
    expect(
      screen.getByRole("link", { name: /^Failing services/ }).getAttribute("aria-current"),
    ).toBe("page");
    expect(
      screen.getByRole("tab", { name: "Failing services" }).getAttribute("aria-selected"),
    ).toBe("true");
  });

  it("shows Add environment on the Environments tab only", async () => {
    renderAt("/environments?tab=failing");
    await screen.findByRole("tab", { name: "Failing services" });
    expect(screen.queryByRole("button", { name: "Add environment" })).toBeNull();
    cleanup();
    renderAt("/environments");
    expect(await screen.findByRole("button", { name: "Add environment" })).toBeTruthy();
  });

  it("reads the tab and the filter from the address and drops what it does not know", async () => {
    const r = renderAt(
      `/environments?tab=failing&env=${enc([A, "not-an-id"])}&show=problems&sort=name`,
    );
    await screen.findByRole("tab", { name: "Failing services" });
    expect(search(r)).toEqual({ tab: "failing", env: [A] });
    cleanup();
    const bogus = renderAt("/environments?tab=bogus&env=nope");
    await screen.findByRole("table");
    expect(search(bogus)).toEqual({ tab: "environments" });
  });

  it("puts the Updates tab's Show chip in the address and leaves the default out", async () => {
    const asked: string[] = [];
    fleetUpdates = (query) => {
      asked.push(query.get("show") ?? "");
      return Response.json(EMPTY_UPDATES);
    };
    const r = renderAt("/environments?tab=updates");
    fireEvent.click(await screen.findByRole("radio", { name: /^Problems/ }));
    await waitFor(() => expect(search(r)).toEqual({ tab: "updates", show: "problems" }));
    await waitFor(() => expect(asked).toContain("problems"));
    fireEvent.click(screen.getByRole("radio", { name: /^Updates/ }));
    await waitFor(() => expect(search(r)).toEqual({ tab: "updates" }));
    expect(asked[0]).toBe("updates");
  });

  it("keeps the sort only on the Environments tab", async () => {
    const r = renderAt("/environments?tab=pools&sort=name&dir=asc");
    await screen.findByRole("tab", { name: "Address pools" });
    expect(search(r)).toEqual({ tab: "pools" });
  });

  it("replaces the history entry on a tab switch, keeps the filter and drops the sort", async () => {
    const r = renderAt(`/environments?sort=name&dir=asc&env=${enc([A])}`);
    await screen.findByRole("table");
    const before = r.history.length;
    fireEvent.mouseDown(screen.getByRole("tab", { name: "Failing services" }));
    fireEvent.click(screen.getByRole("tab", { name: "Failing services" }));
    await waitFor(() => expect(search(r)).toEqual({ tab: "failing", env: [A] }));
    expect(r.history.length).toBe(before);
    expect(await screen.findByRole("button", { name: "Environment: alpha" })).toBeTruthy();
  });

  it("sets env in the address when two environments are picked, and Clear removes it", async () => {
    const r = renderAt("/environments?tab=failing");
    fireEvent.click(await screen.findByRole("button", { name: "Environment: All" }));
    fireEvent.click(await screen.findByRole("option", { name: "alpha" }));
    await waitFor(() => expect(search(r).env).toEqual([A]));
    expect(await screen.findByRole("button", { name: "Environment: alpha" })).toBeTruthy();
    fireEvent.click(screen.getByRole("option", { name: "bravo" }));
    await waitFor(() => expect(search(r).env).toEqual([A, B]));
    expect(await screen.findByRole("button", { name: "2 environments" })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Clear" }));
    await waitFor(() => expect(search(r).env).toBeUndefined());
    expect(await screen.findByRole("button", { name: "Environment: All" })).toBeTruthy();
  });

  it("asks the server for the chosen environments only", async () => {
    const asked: string[] = [];
    fleetFailing = (query) => {
      asked.push(query.getAll("env").join(","));
      return Response.json({ items: [], notIncluded: [], readAt: SUMMARY.readAt });
    };
    renderAt(`/environments?tab=failing&env=${enc([A, B])}`);
    await screen.findByText("Nothing is failing");
    expect(asked).toContain(`${A},${B}`);
  });

  it("stores nothing in the browser", async () => {
    const before = localStorage.length + sessionStorage.length;
    const r = renderAt("/environments");
    fireEvent.click(await screen.findByRole("link", { name: /^Failing services/ }));
    await waitFor(() => expect(search(r).tab).toBe("failing"));
    expect(localStorage.length + sessionStorage.length).toBe(before);
  });

  it("shows a dash and Couldn't read this on the tiles when the summary fails, and the table stays", async () => {
    fleetSummary = () => new Response("boom", { status: 500 });
    renderAt("/environments");
    expect(
      await screen.findByRole("link", { name: /^Failing services ?— ?Couldn't read this\.$/ }),
    ).toBeTruthy();
    expect(
      screen.getByRole("link", { name: /^Environments ?— ?Couldn't read this\.$/ }),
    ).toBeTruthy();
    expect(await screen.findByRole("table")).toBeTruthy();
  });

  it("shows no tiles or tabs when there is no environment yet", async () => {
    list = () => Response.json({ items: [] });
    renderAt("/environments");
    await screen.findByText("No environments yet");
    expect(screen.queryByRole("tablist")).toBeNull();
    expect(screen.queryByRole("link", { name: /^Failing services/ })).toBeNull();
  });
});
