// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { setCsrfToken } from "@/api/client";
import { queryClient } from "@/api/query-client";
import { containersQuery, LIST_REFETCH_MS } from "@/api/resources";
import { routeTree } from "@/routeTree.gen";
import "@/i18n";

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

const NETWORKS = {
  items: [
    {
      id: "n1",
      name: "bridge",
      driver: "bridge",
      scope: "local",
      subnets: ["172.17.0.0/16"],
      created: ago(10 * 86_400_000),
    },
    {
      id: "n2",
      name: "host",
      driver: "host",
      scope: "local",
      subnets: [],
      created: ago(10 * 86_400_000),
    },
  ],
  total: 2,
  truncated: false,
};

let answers: Record<string, () => Response | Promise<Response>>;
let sent: string[];

function stubApi() {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      sent.push(path);
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
      if (path === "/api/environments") return Response.json({ items: [LAB] });
      if (path === "/api/env/e1/summary") return Response.json(SUMMARY);
      const answer = answers[path];
      return answer ? answer() : Response.json({}, { status: 404 });
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

beforeEach(() => {
  sent = [];
  setCsrfToken(null);
  me = {
    user: { id: "u1", username: "admin", role: "admin" },
    csrfToken: "csrf-1",
    permissions: { global: [], environments: { e1: ["environments.read", ...READS] } },
  };
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
  it("shows Name, State, Image, Created, Status and Ports in the server's order", async () => {
    renderAt("/env/e1/containers");
    await screen.findByRole("heading", { name: "Containers" });
    const headers = (await screen.findAllByRole("columnheader")).map((h) => h.textContent);
    expect(headers).toEqual(["Name", "State", "Image", "Created", "Status", "Ports"]);
    const rows = (await screen.findAllByRole("row")).slice(1);
    expect(rows).toHaveLength(3);
    const first = cellsOf(rows[0] as HTMLElement);
    expect(first[0]).toContain("bayplan");
    expect(first[1]).toBe("Running");
    expect(first[2]).toBe("ghcr.io/ianglennon/bayplan:latest");
    expect(first[3]).toBe("3 h ago");
    expect(first[4]).toBe("Up 3 hours");
    expect(first[5]).toBe("0.0.0.0:7480->7480/tcp");
    const names = rows.map((r) => cellsOf(r as HTMLElement)[0]);
    expect(names[0]).toContain("bayplan");
    expect(names[1]).toContain("db");
    expect(names[2]).toContain("old-job");
    expect(screen.getByText("3 containers")).toBeTruthy();
  });

  it("badges Bayplan's own container row and no other (D-27)", async () => {
    renderAt("/env/e1/containers");
    await screen.findByText("old-job");
    const rows = screen.getAllByRole("row").slice(1);
    expect(cellsOf(rows[0] as HTMLElement)[0]).toContain("Bayplan (this instance)");
    expect(cellsOf(rows[1] as HTMLElement)[0]).not.toContain("Bayplan (this instance)");
  });

  it("pairs every state dot with its text and renders a dash for a missing created time and no ports", async () => {
    renderAt("/env/e1/containers");
    await screen.findByText("old-job");
    const rows = screen.getAllByRole("row").slice(1);
    expect(cellsOf(rows[1] as HTMLElement)[1]).toBe("Restarting");
    const last = cellsOf(rows[2] as HTMLElement);
    expect(last[1]).toBe("Exited");
    expect(last[3]).toBe("—");
    expect(last[5]).toBe("—");
    expect(cellsOf(rows[1] as HTMLElement)[5]).toBe("5432/tcp, 5433/tcp");
  });

  it("has no checkboxes, row actions, menus or links in the table (D-56)", async () => {
    renderAt("/env/e1/containers");
    await screen.findByText("old-job");
    const table = screen.getByRole("table");
    expect(within(table).queryByRole("checkbox")).toBeNull();
    expect(within(table).queryByRole("button")).toBeNull();
    expect(within(table).queryByRole("link")).toBeNull();
    expect(within(table).queryByRole("menu")).toBeNull();
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
    me = {
      user: { id: "u1", username: "viewer", role: "user" },
      csrfToken: "csrf-1",
      permissions: { global: [], environments: { e1: ["environments.read"] } },
    };
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
  it("shows Name, Driver, Scope, Subnet and Created, with a dash for no subnet", async () => {
    renderAt("/env/e1/networks");
    await screen.findByText("172.17.0.0/16");
    const headers = (await screen.findAllByRole("columnheader")).map((h) => h.textContent);
    expect(headers).toEqual(["Name", "Driver", "Scope", "Subnet", "Created"]);
    const rows = screen.getAllByRole("row").slice(1);
    const bridge = cellsOf(rows[0] as HTMLElement);
    expect(bridge.slice(0, 4)).toEqual(["bridge", "bridge", "local", "172.17.0.0/16"]);
    // Ten days old: an absolute date, not "10 d ago" (D-66).
    expect(bridge[4]).toMatch(/2026/);
    expect(cellsOf(rows[1] as HTMLElement)[3]).toBe("—");
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
    me = {
      user: { id: "u1", username: "viewer", role: "user" },
      csrfToken: "csrf-1",
      permissions: { global: [], environments: { e1: ["environments.read", "images.read"] } },
    };
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
    me = {
      user: { id: "u1", username: "viewer", role: "user" },
      csrfToken: "csrf-1",
      permissions: { global: [], environments: { e1: ["environments.read", "networks.read"] } },
    };
    renderAt("/env/e1");
    await screen.findByRole("heading", { name: "Lab host" });
    const sidebar = document.querySelector('[data-slot="sidebar"]') as HTMLElement;
    expect(within(sidebar).queryByRole("link", { name: "Containers" })).toBeNull();
    expect(within(sidebar).getByRole("link", { name: "Networks" })).toBeTruthy();
  });
});
