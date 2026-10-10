// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { setCsrfToken } from "@/api/client";
import { queryClient } from "@/api/query-client";
import { containersQuery, LIST_REFETCH_MS, listKey } from "@/api/resources";
import { nodeRowKey } from "@/features/resources/swarm-lists";
import { routeTree } from "@/routeTree.gen";
import "@/i18n";
import { makeMe } from "@/test-me";

// The four read-only lists behind the dashboard tiles (plan 20, ENV-07, D-56,
// D-66): the real route tree against stubbed API answers.

const READS = ["containers.read", "images.read", "volumes.read", "networks.read"];

let me: unknown;

const LAB = {
  id: "e1",
  name: "Lab host",
  kind: "local",
  type: "standalone",
  enabled: true,
  isSelfHost: true,
  createdAt: "2026-10-01T10:00:00Z",
  engine: { name: "Docker Engine", version: "29.8.2", apiVersion: "1.54" },
  status: { state: "online" },
};

const SUMMARY = {
  environment: { id: "e1", name: "Lab host", type: "standalone", status: { state: "online" } },
  engine: {
    name: "Docker Engine",
    version: "29.8.2",
    apiVersion: "1.54",
    os: "linux",
    arch: "amd64",
    storageDriver: "overlayfs",
    ncpu: 8,
    memTotal: 17179869184,
  },
  counts: {
    containers: 3,
    containersRunning: 1,
    containersStopped: 2,
    images: 2,
    volumes: 1,
    networks: 2,
  },
  isWorker: false,
  selfContainerId: "abc123def456",
  fetchedAt: "2026-10-01T10:00:00Z",
};

const ago = (ms: number) => new Date(Date.now() - ms).toISOString();

const CONTAINERS = {
  items: [
    {
      id: "abc123def456aaaa",
      name: "bayplan",
      state: "running",
      status: "Up 3 hours",
      image: "ghcr.io/ianglennon/bayplan:latest",
      created: ago(3 * 3600_000),
      ports: ["0.0.0.0:7480->7480/tcp"],
      isSelf: true,
    },
    {
      id: "ccc",
      name: "db",
      state: "restarting",
      status: "Restarting (1) 5 seconds ago",
      image: "postgres:17",
      created: ago(2 * 86_400_000),
      ports: ["5432/tcp", "5433/tcp"],
      isSelf: false,
    },
    {
      id: "ddd",
      name: "old-job",
      state: "exited",
      status: "Exited (0) 2 days ago",
      image: "alpine",
      ports: null,
      isSelf: false,
    },
  ],
  total: 3,
  truncated: false,
};

const IMAGES = {
  items: [
    {
      id: "sha256:0123456789abcdef0123",
      shortId: "0123456789ab",
      repoTags: ["nginx:1.27", "nginx:latest"],
      size: 734003200,
      created: ago(60_000),
    },
    {
      id: "sha256:fedcba9876543210",
      shortId: "fedcba987654",
      repoTags: ["<none>"],
      size: 1536,
      created: ago(30_000),
    },
  ],
  total: 2,
  truncated: false,
};

const VOLUMES = { items: [{ name: "pgdata", driver: "local" }], total: 1, truncated: false };

// What every network row carries besides its columns' own values (plan 04-13).
const NO_USE = {
  builtIn: false,
  attachedContainers: 0,
  attachedServices: 0,
  owner: "",
  ownerKind: "",
  internal: false,
  attachable: false,
  ipv6: false,
};

const NETWORKS = {
  items: [
    {
      ...NO_USE,
      id: "n1",
      name: "bridge",
      driver: "bridge",
      scope: "local",
      subnets: ["172.17.0.0/16"],
      created: ago(10 * 86_400_000),
      builtIn: true,
      attachedContainers: 2,
      owner: "shop",
      ownerKind: "stack",
    },
    {
      ...NO_USE,
      id: "n2",
      name: "host",
      driver: "host",
      scope: "local",
      subnets: [],
      created: ago(10 * 86_400_000),
      builtIn: true,
    },
  ],
  total: 2,
  truncated: false,
};

let answers: Record<string, (url: URL) => Response | Promise<Response>>;
let sent: string[];
// Every API request's path and query, to see which node a list asked for.
let sentUrls: string[];
// What /api/environments answers; a swarm test swaps it.
let envItem: Record<string, unknown>;

function stubApi() {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      const path = url.pathname;
      sent.push(path);
      sentUrls.push(`${path}${url.search}`);
      if (path === "/api/instance")
        return Response.json({ instanceName: "Lab", needsSetup: false, rememberDays: 30 });
      if (path === "/api/me") return Response.json(me);
      if (path === "/api/version")
        return Response.json({
          commit: "",
          display: "v0.0.0",
          sourceTreeUrl: "https://example.test/tree",
          sourceUrl: "https://example.test",
          version: "v0.0.0",
        });
      if (path === "/api/environments") return Response.json({ items: [envItem] });
      if (path === "/api/env/e1/summary") return Response.json(SUMMARY);
      const answer = answers[path];
      return answer ? answer(url) : Response.json({}, { status: 404 });
    }),
  );
}

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

function cellsOf(row: HTMLElement): string[] {
  return within(row)
    .getAllByRole("cell")
    .map((c) => c.textContent ?? "");
}

// sameKeyWarnings installs a pass-through spy on console.error, so other errors
// still print, and returns it. Read the duplicate-key warnings with
// sameKeyWarningsOf(spy). Restore the spy with mockRestore in a finally block, as
// the file's afterEach only unstubs globals and a leaked spy would capture later
// tests' output (CR-D-01).
function sameKeyWarnings() {
  return vi.spyOn(console, "error");
}

function sameKeyWarningsOf(spy: ReturnType<typeof sameKeyWarnings>): string[] {
  return spy.mock.calls.map((c) => String(c[0])).filter((m) => m.includes("same key"));
}

beforeEach(() => {
  sent = [];
  sentUrls = [];
  envItem = LAB;
  setCsrfToken(null);
  me = makeMe({
    instanceAdmin: true,
    permissions: { global: [], environments: { e1: ["environments.read", ...READS] } },
  });
  answers = {
    "/api/env/e1/containers": () => Response.json(CONTAINERS),
    "/api/env/e1/images": () => Response.json(IMAGES),
    "/api/env/e1/volumes": () => Response.json(VOLUMES),
    "/api/env/e1/networks": () => Response.json(NETWORKS),
  };
  stubApi();
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  queryClient.clear();
  window.localStorage.clear();
  document.title = "";
});

describe("containers list", () => {
  it("shows Name, State, Image, Ports and Created, then the actions column, sorted by name", async () => {
    renderAt("/env/e1/containers?group=none");
    await screen.findByRole("heading", { name: "Containers" });
    const headers = (await screen.findAllByRole("columnheader")).map((h) => h.textContent);
    // The last column is the row menu; its header is for assistive technology only.
    expect(headers).toEqual(["", "Name", "State", "Image", "Ports", "Created", "Actions"]);
    const rows = (await screen.findAllByRole("row")).slice(1);
    expect(rows).toHaveLength(3);
    const first = cellsOf(rows[0] as HTMLElement).slice(1);
    expect(first[0]).toContain("bayplan");
    expect(first[1]).toBe("Running");
    expect(first[2]).toBe("ghcr.io/ianglennon/bayplan:latest");
    expect(first[3]).toBe("0.0.0.0:7480->7480/tcp");
    expect(first[4]).toBe("3 h ago");
    const names = rows.map((r) => cellsOf(r as HTMLElement)[1]);
    expect(names[0]).toContain("bayplan");
    expect(names[1]).toContain("db");
    expect(names[2]).toContain("old-job");
    expect(screen.getByText("3 containers")).toBeTruthy();
  });

  it("badges Bayplan's own container row and no other (D-27)", async () => {
    renderAt("/env/e1/containers?group=none");
    await screen.findByText("old-job");
    const rows = screen.getAllByRole("row").slice(1);
    expect(cellsOf(rows[0] as HTMLElement).slice(1)[0]).toContain("Bayplan (this instance)");
    expect(cellsOf(rows[1] as HTMLElement).slice(1)[0]).not.toContain("Bayplan (this instance)");
  });

  it("pairs every state dot with its text and renders a dash for a missing created time and no ports", async () => {
    renderAt("/env/e1/containers?group=none");
    await screen.findByText("old-job");
    const rows = screen.getAllByRole("row").slice(1);
    expect(cellsOf(rows[1] as HTMLElement).slice(1)[1]).toBe("Restarting");
    const last = cellsOf(rows[2] as HTMLElement).slice(1);
    expect(last[1]).toBe("Exited");
    expect(last[4]).toBe("—");
    expect(last[3]).toBe("—");
    expect(cellsOf(rows[1] as HTMLElement).slice(1)[3]).toBe("5432/tcp, 5433/tcp");
  });

  it("has a checkbox for each row and one for all, a link for each name, and its only other buttons are the closed row action menus (D-56, plan 04-08, 04-10)", async () => {
    renderAt("/env/e1/containers?group=none");
    await screen.findByText("old-job");
    const table = screen.getByRole("table");
    expect(within(table).getAllByRole("checkbox")).toHaveLength(4);
    // The only links are the names, which open the container (plan 04-10).
    const links = within(table).getAllByRole("link");
    expect(links).toHaveLength(3); // one per row; the fourth checkbox is Select all
    for (const l of links) expect(l.getAttribute("href")).toMatch(/^\/env\/e1\/containers\/\w+/);
    expect(within(table).queryByRole("menu")).toBeNull();
    // The sortable headers are buttons too (plan 04-07); the rows hold only menus.
    const buttons = within(table)
      .getAllByRole("button")
      .filter((b) => b.closest("td"));
    expect(buttons.length).toBeGreaterThan(0);
    for (const b of buttons) {
      if (b.getAttribute("role") === "checkbox") continue;
      expect(b.getAttribute("aria-label")).toMatch(/^Actions for /);
    }
  });

  it("sets the tab title to the resource and the instance name", async () => {
    renderAt("/env/e1/containers");
    await screen.findByText("old-job");
    await waitFor(() => expect(document.title).toBe("Containers · Lab"));
  });

  it("refetches every 30 seconds under the documented query key", () => {
    const q = containersQuery("e1");
    expect(q.queryKey).toEqual(["env", "e1", "containers"]);
    expect(q.refetchInterval).toBe(LIST_REFETCH_MS);
    expect(LIST_REFETCH_MS).toBe(30_000);
  });

  it("shows the no-permission page, and asks the server for nothing, without containers.read", async () => {
    me = makeMe({
      id: "u1",
      permissions: { global: [], environments: { e1: ["environments.read"] } },
    });
    renderAt("/env/e1/containers");
    await screen.findByText("You don't have permission to view this page");
    expect(sent).not.toContain("/api/env/e1/containers");
  });

  it("shows 'No containers' and the environment name when there are none", async () => {
    answers["/api/env/e1/containers"] = () =>
      Response.json({ items: [], total: 0, truncated: false });
    renderAt("/env/e1/containers");
    await screen.findByText("No containers");
    expect(screen.getByText("Nothing to show on Lab host right now.")).toBeTruthy();
    expect(screen.getByText("0 containers")).toBeTruthy();
  });

  it("shows an inline error with Retry while the shell stays usable, then recovers", async () => {
    let ok = false;
    answers["/api/env/e1/containers"] = () =>
      ok ? Response.json(CONTAINERS) : Response.json({ title: "boom" }, { status: 500 });
    renderAt("/env/e1/containers");
    await screen.findByText("Couldn't load containers.", {}, { timeout: 4000 });
    expect(screen.getByText("Docker may be unreachable. Try again.")).toBeTruthy();
    expect(screen.getByRole("link", { name: "Dashboard" })).toBeTruthy();
    ok = true;
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await screen.findByText("old-job");
  });

  it("puts the search in the URL 250 ms after typing, replacing the history entry, and asks the server for it", async () => {
    const router = renderAt("/env/e1/containers?group=none");
    await screen.findByText("old-job");
    const length = window.history.length;
    answers["/api/env/e1/containers"] = (url) =>
      Response.json({
        items: CONTAINERS.items.filter((c) => c.name.includes(url.searchParams.get("q") ?? "")),
        total: 1,
        truncated: false,
      });
    fireEvent.change(screen.getByRole("searchbox", { name: "Search containers" }), {
      target: { value: "old" },
    });
    expect(router.state.location.search).toEqual({ group: "none" });
    await waitFor(() => expect(router.state.location.search).toEqual({ group: "none", q: "old" }), {
      timeout: 2000,
    });
    await waitFor(() => expect(sentUrls).toContain("/api/env/e1/containers?q=old"));
    expect(window.history.length).toBe(length);
    await waitFor(() => expect(screen.queryByText("bayplan")).toBeNull());
    fireEvent.click(screen.getByRole("button", { name: "Clear search" }));
    await waitFor(() => expect(router.state.location.search).toEqual({ group: "none" }));
  });

  it("drops an invalid ?group= and groups by stack or project", async () => {
    renderAt("/env/e1/containers?group=foo");
    await screen.findByText("old-job");
    expect(screen.getByRole("button", { name: "Collapse Ungrouped" })).toBeTruthy();
  });

  it("says how many it left out when the server capped the list", async () => {
    answers["/api/env/e1/containers"] = () =>
      Response.json({ ...CONTAINERS, total: 612, truncated: true });
    renderAt("/env/e1/containers");
    await screen.findByText("old-job");
    expect(screen.getByText(/Showing the first 500 of 612\./)).toBeTruthy();
  });
});

describe("images list", () => {
  it("shows Repository:tag, a short mono ID, Size and Created, with a muted <none> for an untagged image", async () => {
    renderAt("/env/e1/images");
    await screen.findByText("nginx:1.27, nginx:latest");
    const headers = (await screen.findAllByRole("columnheader")).map((h) => h.textContent);
    expect(headers).toEqual(["Repository:tag", "Image ID", "Size", "Created"]);
    const rows = screen.getAllByRole("row").slice(1);
    expect(cellsOf(rows[0] as HTMLElement)).toEqual([
      "nginx:1.27, nginx:latest",
      "0123456789ab",
      "700 MiB",
      "1 min ago",
    ]);
    const untagged = cellsOf(rows[1] as HTMLElement);
    expect(untagged[0]).toBe("<none>");
    expect(untagged[1]).toBe("fedcba987654");
    expect(untagged[2]).toBe("1.5 KiB");
    const none = screen.getByText("<none>");
    expect(none.className).toContain("text-muted-foreground");
    expect(screen.getByText("0123456789ab").className).toContain("font-mono");
  });
});

describe("volumes list", () => {
  it("shows Name, Driver and Created, with a dash when the engine gave no time", async () => {
    renderAt("/env/e1/volumes");
    await screen.findByText("pgdata");
    const rows = screen.getAllByRole("row").slice(1);
    expect(cellsOf(rows[0] as HTMLElement)).toEqual(["pgdata", "local", "—"]);
  });
});

describe("networks list", () => {
  it("shows Name to Created, with a dash for no subnet or owner and None when nothing is attached", async () => {
    renderAt("/env/e1/networks");
    await screen.findByText("172.17.0.0/16");
    const headers = (await screen.findAllByRole("columnheader")).map((h) => h.textContent);
    expect(headers).toEqual([
      "Name",
      "Driver",
      "Scope",
      "Subnet",
      "Attached",
      "Owner",
      "Created",
      "Actions",
    ]);
    const rows = screen.getAllByRole("row").slice(1);
    const bridge = cellsOf(rows[0] as HTMLElement);
    expect(bridge.slice(0, 6)).toEqual([
      "bridge",
      "bridge",
      "Local",
      "172.17.0.0/16",
      "2 containers",
      "shop",
    ]);
    // Ten days old: an absolute date, not "10 d ago" (D-66).
    expect(bridge[6]).toMatch(/2026/);
    const host = cellsOf(rows[1] as HTMLElement);
    expect(host.slice(3, 6)).toEqual(["—", "None", "—"]);
  });
});

describe("dashboard tiles and the Resources nav group", () => {
  it("turns the four tiles into links to the lists, and leaves Nodes and Services alone", async () => {
    renderAt("/env/e1");
    await screen.findByRole("heading", { name: "Lab host" });
    const hrefs = {
      Containers: "/env/e1/containers",
      Images: "/env/e1/images",
      Volumes: "/env/e1/volumes",
      Networks: "/env/e1/networks",
    };
    const main = screen.getByRole("region", { name: "Dashboard" });
    for (const [label, href] of Object.entries(hrefs)) {
      const link = within(main).getByRole("link", { name: new RegExp(label) });
      expect(link.getAttribute("href")).toBe(href);
    }
  });

  it("opens a list from its tile", async () => {
    const router = renderAt("/env/e1");
    await screen.findByRole("heading", { name: "Lab host" });
    const main = screen.getByRole("region", { name: "Dashboard" });
    fireEvent.click(within(main).getByRole("link", { name: /Images/ }));
    await screen.findByRole("heading", { name: "Images" });
    expect(router.state.location.pathname).toBe("/env/e1/images");
  });

  it("does not link a tile the user may not read", async () => {
    me = makeMe({
      id: "u1",
      permissions: { global: [], environments: { e1: ["environments.read", "images.read"] } },
    });
    renderAt("/env/e1");
    await screen.findByRole("heading", { name: "Lab host" });
    const main = screen.getByRole("region", { name: "Dashboard" });
    expect(within(main).getAllByRole("link")).toHaveLength(1);
    expect(within(main).getByRole("link", { name: /Images/ })).toBeTruthy();
  });

  it("lists Containers, Images, Volumes and Networks under Resources, marking the open one", async () => {
    renderAt("/env/e1/volumes");
    await screen.findByRole("heading", { name: "Volumes" });
    expect(screen.getByText("Resources")).toBeTruthy();
    const nav = screen
      .getAllByRole("link")
      .filter((l) => l.getAttribute("href")?.startsWith("/env/e1/"));
    expect(nav.map((l) => l.textContent)).toEqual(
      expect.arrayContaining(["Containers", "Images", "Volumes", "Networks"]),
    );
    const volumes = nav.find((l) => l.textContent === "Volumes") as HTMLElement;
    expect(volumes.getAttribute("aria-current")).toBe("page");
    expect(volumes.getAttribute("data-active")).toBe("true");
    const images = nav.find((l) => l.textContent === "Images") as HTMLElement;
    expect(images.getAttribute("aria-current")).toBeNull();
  });

  it("shows only the resources the user may read", async () => {
    me = makeMe({
      id: "u1",
      permissions: { global: [], environments: { e1: ["environments.read", "networks.read"] } },
    });
    renderAt("/env/e1");
    await screen.findByRole("heading", { name: "Lab host" });
    const sidebar = document.querySelector('[data-slot="sidebar"]') as HTMLElement;
    expect(within(sidebar).queryByRole("link", { name: "Containers" })).toBeNull();
    expect(within(sidebar).getByRole("link", { name: "Networks" })).toBeTruthy();
  });
});

// Aggregated swarm lists (plan 22, ENV-02, ENV-06, D-46): a Node column after
// the first column, the Node select held in the URL, the caption across nodes
// and the banner that names the nodes that did not report.
describe("swarm lists", () => {
  const SWARM = {
    ...LAB,
    id: "e1",
    name: "Prod swarm",
    kind: "agent",
    type: "swarm",
    envType: "swarm",
    isSelfHost: false,
    isLocalSocket: false,
    status: { state: "degraded" },
  };

  const row = (id: string, name: string, node: string) => ({
    id,
    name,
    node,
    state: "running",
    status: "Up 1 hour",
    image: "nginx",
    created: ago(3600_000),
    ports: null,
    isSelf: false,
  });

  const ALL_CONTAINERS = [
    row("c1", "web.1", "n1"),
    row("c2", "web.2", "n2"),
    row("c3", "db", "n3"),
  ];

  // The server filters by the node it is asked for and always lists every node
  // that reported, with the ones that did not.
  function containersFor(url: URL) {
    const node = url.searchParams.get("node");
    const items = node ? ALL_CONTAINERS.filter((c) => c.node === node) : ALL_CONTAINERS;
    return Response.json({
      items,
      total: items.length,
      truncated: false,
      nodes: ["n1", "n2", "n3"],
      notReporting: ["n4"],
    });
  }

  beforeEach(() => {
    envItem = SWARM;
    answers["/api/env/e1/containers"] = containersFor;
  });

  function openNodeSelect() {
    fireEvent.keyDown(screen.getByRole("combobox", { name: "Node" }), { key: "Enter" });
  }

  it("shows Node after Name, the caption across the nodes and the banner for a node that did not report", async () => {
    renderAt("/env/e1/containers?group=none");
    await screen.findByText("web.1");
    const headers = screen.getAllByRole("columnheader").map((h) => h.textContent);
    expect(headers).toEqual(["", "Name", "Node", "State", "Image", "Ports", "Created", "Actions"]);
    const rows = screen.getAllByRole("row").slice(1);
    // The page sorts by name, whatever order the server sent.
    expect(cellsOf(rows[0] as HTMLElement).slice(1, 3)).toEqual(["db", "n3"]);
    expect(cellsOf(rows[2] as HTMLElement).slice(1, 3)).toEqual(["web.2", "n2"]);
    expect(screen.getByText("3 containers across 3 nodes")).toBeTruthy();
    expect(
      screen.getByText("1 node isn't reporting: n4. The totals below leave it out."),
    ).toBeTruthy();
    // The banner sits above the table.
    const banner = screen.getByText(/isn't reporting/);
    expect(
      banner.compareDocumentPosition(screen.getByRole("table")) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
  });

  it("gives the Node column one width, so the table does not reflow as nodes report", async () => {
    renderAt("/env/e1/containers?group=none");
    await screen.findByText("web.1");
    const header = screen.getByRole("columnheader", { name: "Node" });
    expect(header.className).toContain("w-56");
    expect(header.className).toContain("min-w-56");
    const cell = within(screen.getAllByRole("row")[1] as HTMLElement).getAllByRole("cell")[2];
    expect(cell?.className).toContain("w-56");
  });

  it("asks for one node, sets ?node= and shows only that node when it is chosen", async () => {
    const router = renderAt("/env/e1/containers");
    await screen.findByText("web.1");
    openNodeSelect();
    fireEvent.keyDown(await screen.findByRole("option", { name: "n2" }), { key: "Enter" });
    await waitFor(() => expect(router.state.location.search).toEqual({ node: "n2" }));
    await screen.findByText("1 container on n2");
    expect(screen.queryByText("web.1")).toBeNull();
    expect(screen.getByText("web.2")).toBeTruthy();
    expect(sentUrls).toContain("/api/env/e1/containers?node=n2");
  });

  it("goes back to all nodes and drops the parameter", async () => {
    const router = renderAt("/env/e1/containers?node=n2");
    await screen.findByText("1 container on n2");
    openNodeSelect();
    fireEvent.keyDown(await screen.findByRole("option", { name: "All nodes" }), { key: "Enter" });
    await screen.findByText("3 containers across 3 nodes");
    expect(router.state.location.search).toEqual({});
  });

  it("reads the node from a link and asks the server for exactly it", async () => {
    renderAt("/env/e1/containers?node=n3");
    await screen.findByText("db");
    expect(sentUrls).toContain("/api/env/e1/containers?node=n3");
    expect(screen.getByRole("combobox", { name: "Node" }).textContent).toContain("n3");
  });

  it("keeps the rows on screen while the next node loads, so the page does not jump", async () => {
    let release: (() => void) | undefined;
    renderAt("/env/e1/containers");
    await screen.findByText("web.1");
    answers["/api/env/e1/containers"] = (url) =>
      new Promise((resolve) => {
        release = () => resolve(containersFor(url));
      });
    openNodeSelect();
    fireEvent.keyDown(await screen.findByRole("option", { name: "n1" }), { key: "Enter" });
    await waitFor(() => expect(release).toBeDefined());
    expect(screen.getByText("web.2")).toBeTruthy();
    expect(screen.queryByRole("table")?.getAttribute("aria-busy")).toBeNull();
    (release as () => void)();
    await waitFor(() => expect(screen.queryByText("web.2")).toBeNull());
  });

  it("says 'Nothing to show on n2 right now.' when the chosen node has no rows", async () => {
    answers["/api/env/e1/containers"] = () =>
      Response.json({
        items: [],
        total: 0,
        truncated: false,
        nodes: ["n1", "n2"],
        notReporting: [],
      });
    renderAt("/env/e1/containers?node=n2");
    await screen.findByText("Nothing to show on n2 right now.");
    expect(screen.getByText("No containers")).toBeTruthy();
    expect(screen.getByRole("combobox", { name: "Node" })).toBeTruthy();
    expect(screen.queryByText(/isn't reporting/)).toBeNull();
  });

  it("shows 8 skeleton rows with the Node column and filter in place while the list loads", async () => {
    answers["/api/env/e1/containers"] = () => new Promise<Response>(() => {});
    renderAt("/env/e1/containers");
    await screen.findByRole("heading", { name: "Containers" });
    await screen.findByRole("columnheader", { name: "Node" });
    expect(screen.getAllByRole("row")).toHaveLength(9);
    expect((screen.getByRole("combobox", { name: "Node" }) as HTMLButtonElement).disabled).toBe(
      false,
    );
  });

  it("puts a swarm-scoped network under Cluster", async () => {
    answers["/api/env/e1/networks"] = () =>
      Response.json({
        items: [
          {
            ...NO_USE,
            id: "n1",
            name: "ingress",
            node: "Cluster",
            driver: "overlay",
            scope: "swarm",
            subnets: ["10.0.0.0/24"],
            created: ago(86_400_000 * 10),
          },
          {
            ...NO_USE,
            id: "n2",
            name: "bridge",
            node: "n1",
            driver: "bridge",
            scope: "local",
            subnets: [],
            created: ago(86_400_000 * 10),
          },
        ],
        total: 2,
        truncated: false,
        nodes: ["Cluster", "n1"],
        notReporting: [],
      });
    renderAt("/env/e1/networks");
    await screen.findByText("ingress");
    const headers = screen.getAllByRole("columnheader").map((h) => h.textContent);
    expect(headers).toEqual([
      "Name",
      "Node",
      "Driver",
      "Scope",
      "Subnet",
      "Attached",
      "Owner",
      "Created",
      "Actions",
    ]);
    const rows = screen.getAllByRole("row").slice(1);
    expect(cellsOf(rows[0] as HTMLElement).slice(0, 2)).toEqual(["ingress", "Cluster"]);
    expect(cellsOf(rows[1] as HTMLElement).slice(0, 2)).toEqual(["bridge", "n1"]);
    // Cluster is a choice in the filter but not a node in the caption.
    expect(screen.getByText("2 networks across 1 node")).toBeTruthy();
    openNodeSelect();
    expect(screen.getAllByRole("option").map((o) => o.textContent)).toEqual([
      "All nodes",
      "Cluster",
      "n1",
    ]);
  });

  it("gives images and volumes a Node column after their first column", async () => {
    answers["/api/env/e1/images"] = () =>
      Response.json({
        items: [
          {
            id: "sha256:0123456789abcdef0123",
            shortId: "0123456789ab",
            node: "n1",
            repoTags: ["nginx:1.27"],
            size: 1024,
            created: ago(60_000),
          },
        ],
        total: 1,
        truncated: false,
        nodes: ["n1"],
        notReporting: [],
      });
    answers["/api/env/e1/volumes"] = () =>
      Response.json({
        items: [{ name: "pgdata", node: "n2", driver: "local" }],
        total: 1,
        truncated: false,
        nodes: ["n2"],
        notReporting: [],
      });
    renderAt("/env/e1/images");
    await screen.findByText("nginx:1.27");
    expect(screen.getAllByRole("columnheader").map((h) => h.textContent)).toEqual([
      "Repository:tag",
      "Node",
      "Image ID",
      "Size",
      "Created",
    ]);
    cleanup();
    renderAt("/env/e1/volumes");
    await screen.findByText("pgdata");
    expect(screen.getAllByRole("columnheader").map((h) => h.textContent)).toEqual([
      "Name",
      "Node",
      "Driver",
      "Created",
    ]);
  });

  it("renders the same image on two nodes as two rows with no duplicate-key warning (CR-D-01)", async () => {
    const agentImage = (node: string) => ({
      id: "sha256:a9e1c0de",
      shortId: "a9e1c0de",
      node,
      repoTags: ["ghcr.io/ianglennon/bayplan-agent:0.1.0"],
      size: 1024,
      created: ago(60_000),
    });
    const imagesOn = (nodes: string[]) => () =>
      Response.json({
        items: nodes.map(agentImage),
        total: nodes.length,
        truncated: false,
        nodes,
        notReporting: [],
      });
    answers["/api/env/e1/images"] = imagesOn(["n1", "n2"]);
    const spy = sameKeyWarnings();
    try {
      renderAt("/env/e1/images");
      await screen.findAllByText("ghcr.io/ianglennon/bayplan-agent:0.1.0");
      let rows = screen.getAllByRole("row").slice(1);
      expect(rows.map((r) => cellsOf(r as HTMLElement)[1])).toEqual(["n1", "n2"]);

      answers["/api/env/e1/images"] = imagesOn(["n2", "n3"]);
      await queryClient.invalidateQueries({ queryKey: listKey("e1", "images") });
      await waitFor(() => {
        rows = screen.getAllByRole("row").slice(1);
        expect(rows.map((r) => cellsOf(r as HTMLElement)[1])).toEqual(["n2", "n3"]);
      });
      expect(sameKeyWarningsOf(spy)).toEqual([]);
    } finally {
      spy.mockRestore();
    }
  });

  it("renders the same volume name on two nodes as two rows with no duplicate-key warning (CR-D-01)", async () => {
    answers["/api/env/e1/volumes"] = () =>
      Response.json({
        items: [
          { name: "pgdata", node: "n1", driver: "local" },
          { name: "pgdata", node: "n2", driver: "local" },
        ],
        total: 2,
        truncated: false,
        nodes: ["n1", "n2"],
        notReporting: [],
      });
    const spy = sameKeyWarnings();
    try {
      renderAt("/env/e1/volumes");
      await screen.findAllByText("pgdata");
      const rows = screen.getAllByRole("row").slice(1);
      expect(rows.map((r) => cellsOf(r as HTMLElement)[1])).toEqual(["n1", "n2"]);
      expect(sameKeyWarningsOf(spy)).toEqual([]);
    } finally {
      spy.mockRestore();
    }
  });

  it("keys networks and containers by node too, so the same id on two nodes gives two rows", async () => {
    answers["/api/env/e1/networks"] = () =>
      Response.json({
        items: ["n1", "n2"].map((node) => ({
          ...NO_USE,
          id: "b1",
          name: "bridge",
          node,
          driver: "bridge",
          scope: "local",
          subnets: [],
          created: ago(86_400_000),
        })),
        total: 2,
        truncated: false,
        nodes: ["n1", "n2"],
        notReporting: [],
      });
    answers["/api/env/e1/containers"] = () =>
      Response.json({
        items: [row("c1", "web", "n1"), row("c1", "web", "n2")],
        total: 2,
        truncated: false,
        nodes: ["n1", "n2"],
        notReporting: [],
      });
    const spy = sameKeyWarnings();
    try {
      renderAt("/env/e1/networks");
      await screen.findAllByText("bridge");
      let rows = screen.getAllByRole("row").slice(1);
      expect(rows.map((r) => cellsOf(r as HTMLElement)[1])).toEqual(["n1", "n2"]);
      cleanup();
      renderAt("/env/e1/containers?group=none");
      await screen.findAllByText("web");
      rows = screen.getAllByRole("row").slice(1);
      expect(rows.map((r) => cellsOf(r as HTMLElement)[2])).toEqual(["n1", "n2"]);
      expect(sameKeyWarningsOf(spy)).toEqual([]);
    } finally {
      spy.mockRestore();
    }
  });

  it("keeps standalone keys unique when rows carry no node", async () => {
    envItem = LAB;
    const spy = sameKeyWarnings();
    try {
      renderAt("/env/e1/images");
      await screen.findByText("nginx:1.27, nginx:latest");
      expect(screen.getAllByRole("row").slice(1)).toHaveLength(2);
      cleanup();
      renderAt("/env/e1/volumes");
      await screen.findByText("pgdata");
      expect(screen.getAllByRole("row").slice(1)).toHaveLength(1);
      expect(sameKeyWarningsOf(spy)).toEqual([]);
    } finally {
      spy.mockRestore();
    }
  });

  it("shows the banner on every list when a node is missing", async () => {
    for (const resource of ["images", "volumes", "networks"] as const) {
      answers[`/api/env/e1/${resource}`] = () =>
        Response.json({
          items: [],
          total: 0,
          truncated: false,
          nodes: [],
          notReporting: ["n1", "n2"],
        });
    }
    renderAt("/env/e1/images");
    await screen.findByText(/2 nodes aren't reporting: n1, n2\./);
    cleanup();
    renderAt("/env/e1/volumes");
    await screen.findByText(/2 nodes aren't reporting: n1, n2\./);
    cleanup();
    renderAt("/env/e1/networks");
    await screen.findByText(/2 nodes aren't reporting: n1, n2\./);
  });

  it("ignores a node parameter that is too long and sends nothing for it", async () => {
    renderAt(`/env/e1/containers?node=${"x".repeat(200)}`);
    await screen.findByText("web.1");
    expect(sentUrls).toContain("/api/env/e1/containers");
    expect(sentUrls.some((u) => u.includes("node="))).toBe(false);
  });

  it("sends a hostile node parameter as a plain query value and shows it as text", async () => {
    const evil = "<img src=x onerror=alert(1)>";
    renderAt(`/env/e1/containers?node=${encodeURIComponent(evil)}`);
    await screen.findByText(`Nothing to show on ${evil} right now.`, {}, { timeout: 3000 });
    expect(document.querySelector("img[src='x']")).toBeNull();
    expect(sentUrls.some((u) => u.includes(`node=${encodeURIComponent(evil)}`))).toBe(true);
  });

  it("leaves a standalone host without the Node column, the filter and the banner", async () => {
    envItem = LAB;
    answers["/api/env/e1/containers"] = () => Response.json(CONTAINERS);
    renderAt("/env/e1/containers");
    await screen.findByText("old-job");
    expect(screen.queryByRole("combobox", { name: "Node" })).toBeNull();
    expect(screen.queryByRole("columnheader", { name: "Node" })).toBeNull();
    expect(screen.queryByText(/isn't reporting/)).toBeNull();
    expect(screen.getByText("3 containers")).toBeTruthy();
  });
});

describe("nodeRowKey", () => {
  it("joins the node and the id with a separator", () => {
    expect(nodeRowKey("n1", "x")).toBe("n1|x");
  });

  it("differs for the same id on two nodes", () => {
    expect(nodeRowKey("n2", "x")).not.toBe(nodeRowKey("n1", "x"));
  });

  it("keys a row with no node as '|<id>'", () => {
    expect(nodeRowKey(undefined, "x")).toBe("|x");
  });

  it("keys a row with an empty node as '|<id>'", () => {
    expect(nodeRowKey("", "x")).toBe("|x");
  });
});
