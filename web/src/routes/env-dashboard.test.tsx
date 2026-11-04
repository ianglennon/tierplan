// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { setCsrfToken } from "@/api/client";
import { SUMMARY_REFETCH_MS, summaryQuery } from "@/api/environments";
import { queryClient } from "@/api/query-client";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { routeTree } from "@/routeTree.gen";
import "@/i18n";
import { janitorStatus } from "@/test-janitor";
import { makeMe } from "@/test-me";

const ME = makeMe({
  instanceAdmin: true,
  permissions: { global: [], environments: { "*": ["environments.read"] } },
});

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
    containers: 7,
    containersRunning: 5,
    containersStopped: 2,
    images: 12,
    volumes: 0,
    networks: 4,
  },
  isWorker: false,
  selfContainerId: "abc123def456",
  fetchedAt: "2026-10-01T10:00:00Z",
};

let sent: string[] = [];
let environments: () => Response | Promise<Response>;
let summary: () => Response | Promise<Response>;
let recent: string[] = [];
let me: unknown = ME;

function stubApi() {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      sent.push(path);
      if (path === "/api/instance") {
        return Response.json({ instanceName: "Lab", needsSetup: false, rememberDays: 30 });
      }
      if (path === "/api/me") return Response.json(me);
      if (path === "/api/version") {
        return Response.json({
          commit: "",
          display: "v0.0.0",
          sourceTreeUrl: "https://example.test/tree",
          sourceUrl: "https://example.test",
          version: "v0.0.0",
        });
      }
      if (path === "/api/environments") return environments();
      if (path === "/api/me/recent-environments") {
        return Response.json({ environmentIds: recent });
      }
      if (path === "/api/env/e1/summary") return summary();
      if (path === "/api/env/e1/events") return Response.json({ items: [] });
      if (path === "/api/env/e1/janitor") return Response.json(janitorStatus());
      return Response.json({}, { status: 404 });
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

// tile finds a count tile by its label and returns the tile element.
function tile(label: string): HTMLElement {
  // Inside the tiles, since a nav item of the same name is a span as well.
  const tiles = screen.getByRole("region", { name: "Dashboard" });
  const el = within(tiles).getByText(label, { selector: "span" }).parentElement;
  if (!el) throw new Error(`no tile ${label}`);
  return el;
}

beforeEach(() => {
  sent = [];
  setCsrfToken(null);
  environments = () => Response.json({ items: [LAB] });
  summary = () => Response.json(SUMMARY);
  recent = [];
  me = ME;
  stubApi();
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  queryClient.clear();
  window.localStorage.clear();
  document.title = "";
});

describe("home redirect", () => {
  it("sends / to the last-used environment's dashboard (ENV-07, D-31)", async () => {
    recent = ["e1"];
    const router = renderAt("/");
    await screen.findByRole("heading", { name: "Lab host" });
    expect(router.state.location.pathname).toBe("/env/e1");
  });

  it("sends / to the overview when there is no last-used environment", async () => {
    const router = renderAt("/");
    await screen.findByRole("heading", { level: 1, name: "Environments" });
    expect(router.state.location.pathname).toBe("/environments");
  });

  it("ends on the overview's empty state when no environment exists", async () => {
    environments = () => Response.json({ items: [] });
    const router = renderAt("/");
    expect(await screen.findByText("No environments yet")).toBeTruthy();
    expect(router.state.location.pathname).toBe("/environments");
  });

  it("ends on the overview, which says so, when the list cannot be read", async () => {
    environments = () => new Response("boom", { status: 500 });
    const router = renderAt("/");
    expect(await screen.findByText("Couldn't load environments.")).toBeTruthy();
    expect(router.state.location.pathname).toBe("/environments");
  });
});

describe("dashboard", () => {
  it("holds a 64-character name to two lines and keeps the whole name in a title", async () => {
    const long = "n".repeat(64);
    environments = () => Response.json({ items: [{ ...LAB, name: long }] });
    summary = () =>
      Response.json({ ...SUMMARY, environment: { ...SUMMARY.environment, name: long } });
    renderAt("/env/e1");
    const heading = await screen.findByRole("heading", { name: long });
    expect(heading.className).toContain("line-clamp-2");
    expect(heading.getAttribute("title")).toBe(long);
  });

  it("shows the header, the four tiles and the engine card from one summary request", async () => {
    renderAt("/env/e1");
    const heading = await screen.findByRole("heading", { name: "Lab host" });
    const header = heading.parentElement as HTMLElement;
    expect(within(header).getByText("Standalone")).toBeTruthy();
    expect(within(header).getByText("Online")).toBeTruthy();
    expect(within(header).getByText("TierPlan (this instance)")).toBeTruthy();

    await waitFor(() => expect(within(tile("Containers")).getByText("7")).toBeTruthy());
    expect(within(tile("Containers")).getByText("5 running · 2 stopped")).toBeTruthy();
    expect(within(tile("Images")).getByText("12")).toBeTruthy();
    expect(within(tile("Networks")).getByText("4")).toBeTruthy();
    // Zero is a number, never blank.
    expect(within(tile("Volumes")).getByText("0")).toBeTruthy();
    expect(screen.queryByText("Nodes", { selector: "span" })).toBeNull();
    expect(screen.queryByText("Services", { selector: "span" })).toBeNull();

    expect(screen.getByText("Docker engine")).toBeTruthy();
    expect(screen.getByText("29.8.2")).toBeTruthy();
    expect(screen.getByText("1.54")).toBeTruthy();
    expect(screen.getByText("linux / amd64")).toBeTruthy();
    expect(screen.getByText("overlayfs")).toBeTruthy();
    expect(screen.getByText("8")).toBeTruthy();
    expect(screen.getByText("16.0 GiB")).toBeTruthy();

    // The layout and the page share one cache entry, so there is one request.
    expect(sent.filter((p) => p === "/api/env/e1/summary")).toHaveLength(1);
    expect(document.title).toBe("Dashboard · Lab");
  });

  it("stacks the engine, address space and janitor cards in that order in the left column", async () => {
    me = makeMe({ instanceAdmin: true });
    renderAt("/env/e1");
    const janitor = await screen.findByRole("region", { name: "Janitor" });
    const engine = (await screen.findByText("Docker engine")).closest("[data-slot=card]");
    const address = screen.getByRole("region", { name: "Address space" });
    expect(engine).not.toBeNull();
    const column = janitor.parentElement as HTMLElement;
    expect(column.contains(address)).toBe(true);
    expect(column.contains(engine)).toBe(true);
    const order = Array.from(column.children);
    const at = (el: Element | null) => order.findIndex((child) => child.contains(el));
    expect(at(engine)).toBeLessThan(at(address));
    expect(at(address)).toBeLessThan(at(janitor));
    expect(await within(janitor).findByText("Hasn't run yet")).toBeTruthy();
  });

  it("refetches the counts every 30 seconds as the polling fallback (D-57)", () => {
    expect(SUMMARY_REFETCH_MS).toBe(30_000);
    expect(summaryQuery("e1").refetchInterval).toBe(30_000);
    expect(summaryQuery("e1").queryKey).toEqual(["env", "e1", "summary"]);
  });

  it("shows Nodes and Services tiles on a swarm manager, without links", async () => {
    summary = () =>
      Response.json({
        ...SUMMARY,
        counts: { ...SUMMARY.counts, nodes: 3, services: 0 },
      });
    renderAt("/env/e1");
    await waitFor(() => expect(within(tile("Nodes")).getByText("3")).toBeTruthy());
    expect(within(tile("Services")).getByText("0")).toBeTruthy();
    // Phase 1 has no destination for any tile yet, so none is a link.
    expect(screen.queryByRole("link", { name: /Nodes|Services|Containers/ })).toBeNull();
  });

  describe("the Restart loops tile (plan 06-35, RES-13, D-15)", () => {
    const swarm = (restartLoops?: { flagged: number; muted: number }) =>
      Response.json({
        ...SUMMARY,
        environment: { ...SUMMARY.environment, type: "swarm-manager" },
        counts: { ...SUMMARY.counts, nodes: 3, services: 5 },
        ...(restartLoops ? { restartLoops } : {}),
      });
    const loops = () => {
      const label = screen.getByText("Restart loops", { selector: "span" });
      return label.closest("a, div") as HTMLElement;
    };

    it("counts the services failing repeatedly, with an offline dot, and links to the loop filter", async () => {
      me = makeMe({
        instanceAdmin: true,
        permissions: { global: [], environments: { "*": ["environments.read", "services.read"] } },
      });
      summary = () => swarm({ flagged: 2, muted: 0 });
      renderAt("/env/e1");
      await waitFor(() => expect(within(loops()).getByText("2")).toBeTruthy());
      expect(within(loops()).getByText("services failing repeatedly")).toBeTruthy();
      expect(loops().querySelector(".bg-status-offline")).not.toBeNull();
      expect(loops().getAttribute("href")).toBe("/env/e1/services?status=loop");
    });

    it("is not a link for someone who may not read services", async () => {
      summary = () => swarm({ flagged: 2, muted: 0 });
      renderAt("/env/e1");
      await waitFor(() => expect(within(loops()).getByText("2")).toBeTruthy());
      expect(loops().getAttribute("href")).toBeNull();
    });

    it("reads None right now at zero, with no dot", async () => {
      summary = () => swarm({ flagged: 0, muted: 0 });
      renderAt("/env/e1");
      await waitFor(() => expect(within(loops()).getByText("0")).toBeTruthy());
      expect(within(loops()).getByText("None right now")).toBeTruthy();
      expect(loops().querySelector(".bg-status-offline")).toBeNull();
    });

    it("says how many are muted", async () => {
      summary = () => swarm({ flagged: 0, muted: 2 });
      renderAt("/env/e1");
      await waitFor(() => expect(within(loops()).getByText("0")).toBeTruthy());
      expect(within(loops()).getByText("None right now · 2 muted")).toBeTruthy();
    });

    it("shows a dash while the swarm has not been read", async () => {
      summary = () => swarm();
      renderAt("/env/e1");
      await waitFor(() => expect(within(tile("Nodes")).getByText("3")).toBeTruthy());
      expect(within(loops()).getByText("—")).toBeTruthy();
    });

    it("is not on a standalone host", async () => {
      renderAt("/env/e1");
      await waitFor(() => expect(within(tile("Images")).getByText("12")).toBeTruthy());
      expect(screen.queryByText("Restart loops", { selector: "span" })).toBeNull();
    });
  });

  it("shows the worker notice on a swarm worker (D-24)", async () => {
    summary = () => Response.json({ ...SUMMARY, isWorker: true });
    renderAt("/env/e1");
    expect(
      await screen.findByText(
        "This host is a swarm worker. Pin TierPlan to a manager node to manage the swarm. Showing node-local data only.",
      ),
    ).toBeTruthy();
  });

  it("badges TierPlan's own host only when the server found its container (D-27)", async () => {
    summary = () => Response.json({ ...SUMMARY, selfContainerId: undefined });
    renderAt("/env/e1");
    await screen.findByRole("heading", { name: "Lab host" });
    await waitFor(() => expect(within(tile("Images")).getByText("12")).toBeTruthy());
    expect(screen.queryByText("TierPlan (this instance)")).toBeNull();
  });

  it("does not badge another host", async () => {
    environments = () => Response.json({ items: [{ ...LAB, isSelfHost: false }] });
    renderAt("/env/e1");
    await waitFor(() => expect(within(tile("Images")).getByText("12")).toBeTruthy());
    expect(screen.queryByText("TierPlan (this instance)")).toBeNull();
  });

  it("ends the tiles with Updates available, from the summary's imageUpdates, linking to the Updates tab", async () => {
    me = makeMe({
      id: "u1",
      permissions: { global: [], environments: { "*": ["environments.read", "containers.read"] } },
    });
    summary = () => Response.json({ ...SUMMARY, imageUpdates: 3 });
    renderAt("/env/e1");
    await waitFor(() => expect(within(tile("Updates available")).getByText("3")).toBeTruthy());
    const el = tile("Updates available");
    expect(el.textContent).toContain("In this environment");
    const tiles = within(screen.getByRole("region", { name: "Dashboard" }));
    const labels = tiles.getAllByText(/./, { selector: "span.text-xs.font-semibold" });
    expect(labels[labels.length - 1]?.textContent).toBe("Updates available");
    expect(el.getAttribute("href")).toContain("tab=updates");
    expect(el.getAttribute("href")).toContain("env=e1");
  });

  it("leaves the Updates available tile out for a person who may not read containers", async () => {
    renderAt("/env/e1");
    await screen.findByRole("heading", { name: "Lab host" });
    expect(screen.queryByText("Updates available")).toBeNull();
  });

  it("shows a real zero as 0 on the Updates available tile", async () => {
    me = makeMe({
      id: "u1",
      permissions: { global: [], environments: { "*": ["environments.read", "containers.read"] } },
    });
    summary = () => Response.json({ ...SUMMARY, imageUpdates: 0 });
    renderAt("/env/e1");
    await waitFor(() => expect(within(tile("Updates available")).getByText("0")).toBeTruthy());
  });

  it("shows a dash and says it could not be read when imageUpdates is absent, never 0", async () => {
    me = makeMe({
      id: "u1",
      permissions: { global: [], environments: { "*": ["environments.read", "containers.read"] } },
    });
    renderAt("/env/e1");
    await waitFor(() => expect(within(tile("Containers")).getByText("7")).toBeTruthy());
    const el = tile("Updates available");
    expect(within(el).getByText("—")).toBeTruthy();
    expect(within(el).getByText("Couldn't read this.")).toBeTruthy();
    expect(within(el).queryByText("0")).toBeNull();
  });

  it("renders a dash for what Docker does not report and lets values wrap", async () => {
    summary = () =>
      Response.json({
        ...SUMMARY,
        engine: { name: "Docker Engine", version: "29.8.2", ncpu: 0, memTotal: 0 },
      });
    renderAt("/env/e1");
    const card = (await screen.findByText("Docker engine")).closest(
      "[data-slot=card]",
    ) as HTMLElement;
    await waitFor(() => expect(within(card).getByText("29.8.2")).toBeTruthy());
    // API version, OS / architecture, storage driver, CPUs and memory.
    expect(within(card).getAllByText("—")).toHaveLength(5);
    expect(within(card).getByText("29.8.2").className).toContain("break-words");
  });

  it("shows a skeleton in place of each value while loading", async () => {
    summary = () => new Promise<Response>(() => {});
    renderAt("/env/e1");
    await screen.findByText("Containers", { selector: "span" });
    expect(tile("Containers").querySelector("[data-slot=skeleton]")).not.toBeNull();
    expect(tile("Containers").textContent).toBe("Containers");
    expect(tile("Images").querySelector("[data-slot=skeleton]")).not.toBeNull();
  });

  it("shows dashes and an inline error with Retry when the summary fails, other cards still render", async () => {
    summary = () => new Response("boom", { status: 500 });
    renderAt("/env/e1");
    expect(await screen.findByText("Couldn't load the dashboard.")).toBeTruthy();
    expect(screen.getByText("Docker may be unreachable. Try again.")).toBeTruthy();
    for (const label of ["Containers", "Images", "Volumes", "Networks"]) {
      expect(within(tile(label)).getByText("—")).toBeTruthy();
    }
    expect(screen.getByText("Docker engine")).toBeTruthy();
    // The shell around it is intact and the page is not a full-page state.
    expect(screen.getByRole("button", { name: "User menu" })).toBeTruthy();

    summary = () => Response.json(SUMMARY);
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(within(tile("Containers")).getByText("7")).toBeTruthy());
    expect(screen.queryByText("Couldn't load the dashboard.")).toBeNull();
  });

  it("marks Dashboard as the current item in the sidebar", async () => {
    renderAt("/env/e1");
    const link = await screen.findByRole("link", { name: "Dashboard" });
    expect(link.getAttribute("aria-current")).toBe("page");
  });
});

// The signed-in layout opens the app's one health stream as well, so the
// environment's own events stream is found by its address.
const feedStream = () =>
  FakeEventSource.instances.find((s) => s.url === "/api/env/e1/events/stream");

describe("events feed (ENV-07, D-57)", () => {
  beforeEach(() => {
    FakeEventSource.reset();
    vi.stubGlobal("EventSource", FakeEventSource);
  });

  it("shows the feed next to the engine card and refreshes the counts when a container event arrives", async () => {
    renderAt("/env/e1");
    await screen.findByRole("heading", { name: "Lab host" });
    expect(await screen.findByText("Recent events")).toBeTruthy();
    expect(screen.getByText("Docker engine")).toBeTruthy();
    await waitFor(() => expect(feedStream()).toBeTruthy());
    expect(FakeEventSource.instances.map((s) => s.url)).toContain("/api/environments/stream");
    expect(within(tile("Containers")).getByText("7")).toBeTruthy();

    summary = () =>
      Response.json({
        ...SUMMARY,
        counts: { ...SUMMARY.counts, containers: 8, containersRunning: 6 },
      });
    const before = sent.filter((p) => p === "/api/env/e1/summary").length;
    feedStream()?.open();
    feedStream()?.send({
      id: "ev1",
      node: "node-1",
      time: "2026-10-02T10:00:00Z",
      type: "container",
      action: "start",
      actorId: "abc",
      name: "web-1",
      attributes: { image: "nginx" },
    });

    await waitFor(() => expect(within(tile("Containers")).getByText("8")).toBeTruthy());
    expect(sent.filter((p) => p === "/api/env/e1/summary").length).toBeGreaterThan(before);
    expect(screen.getByText("Container start")).toBeTruthy();
    expect(screen.getByText("Live")).toBeTruthy();
  });

  it("opens no stream for an environment that is offline", async () => {
    summary = () =>
      Response.json(
        { type: "environment_offline", title: "x", status: 503, reason: "unreachable" },
        { status: 503 },
      );
    renderAt("/env/e1");
    await screen.findByText("Docker isn't reachable");
    expect(feedStream()).toBeUndefined();
  });
});

// A swarm's dashboard (plan 22, ENV-02, ENV-06, D-44, D-46): cluster totals with
// each node's share, the banner for nodes that did not report, manager tiles
// that say when no manager is connected, and the agent rows on the engine card.
describe("swarm dashboard", () => {
  const SWARM = {
    ...LAB,
    name: "Prod swarm",
    kind: "agent",
    type: "swarm",
    envType: "swarm",
    isSelfHost: false,
    isLocalSocket: false,
    status: { state: "online" },
    nodes: { ready: 3, total: 4 },
    agents: { connected: 3, expected: 4 },
    agentVersion: "0.2.0",
  };

  const byNode = [
    { node: "n1", containers: 4, images: 3, volumes: 0, networks: 1 },
    { node: "n2", containers: 2, images: 3, volumes: 0, networks: 1 },
    { node: "n3", containers: 1, images: 2, volumes: 0, networks: 0 },
  ];

  const SWARM_SUMMARY = {
    ...SUMMARY,
    environment: {
      id: "e1",
      name: "Prod swarm",
      type: "swarm-manager",
      status: { state: "online" },
    },
    counts: { ...SUMMARY.counts, nodes: 4, services: 6 },
    byNode,
    notReporting: [],
  };

  beforeEach(() => {
    me = {
      ...ME,
      permissions: {
        global: [],
        environments: {
          "*": [
            "environments.read",
            "containers.read",
            "images.read",
            "volumes.read",
            "networks.read",
          ],
        },
      },
    };
    environments = () => Response.json({ items: [SWARM] });
    summary = () => Response.json(SWARM_SUMMARY);
  });

  // The tiles' own links; the sidebar has links of the same names.
  const tiles = () => within(screen.getByRole("region", { name: "Dashboard" }));

  async function showTooltip(label: string) {
    await screen.findByRole("region", { name: "Dashboard" });
    const link = await tiles().findByRole("link", { name: new RegExp(label) });
    fireEvent.focus(link);
    return screen.findByRole("tooltip");
  }

  it("shows the Swarm badge and how many nodes are ready in the header", async () => {
    renderAt("/env/e1");
    const heading = await screen.findByRole("heading", { name: "Prod swarm" });
    const header = heading.parentElement as HTMLElement;
    expect(within(header).getByText("Swarm")).toBeTruthy();
    expect(within(header).getByText("3 of 4 nodes ready")).toBeTruthy();
  });

  it("has no nodes-ready line on a standalone host", async () => {
    environments = () => Response.json({ items: [LAB] });
    summary = () => Response.json(SUMMARY);
    renderAt("/env/e1");
    await screen.findByRole("heading", { name: "Lab host" });
    expect(screen.queryByText(/nodes ready/)).toBeNull();
  });

  it("names the nodes that did not report above the tiles, with no error", async () => {
    summary = () => Response.json({ ...SWARM_SUMMARY, notReporting: ["n4", "n5"] });
    renderAt("/env/e1");
    const banner = await screen.findByText(
      "2 nodes aren't reporting: n4, n5. The totals below leave them out.",
    );
    const tiles = screen.getByRole("region", { name: "Dashboard" });
    expect(banner.compareDocumentPosition(tiles) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(screen.queryByText("Couldn't load the dashboard.")).toBeNull();
  });

  it("shows no banner when every node reports", async () => {
    renderAt("/env/e1");
    await waitFor(() => expect(within(tile("Containers")).getByText("7")).toBeTruthy());
    expect(screen.queryByText(/isn't reporting|aren't reporting/)).toBeNull();
  });

  it("lists each node's containers in a tooltip on the containers tile", async () => {
    renderAt("/env/e1");
    const tip = await showTooltip("Containers");
    expect(tip.textContent).toBe("n1: 4n2: 2n3: 1");
  });

  it("gives the images and volumes tiles their node breakdown too", async () => {
    renderAt("/env/e1");
    const tip = await showTooltip("Images");
    expect(tip.textContent).toBe("n1: 3n2: 3n3: 2");
  });

  it("shows the swarm-wide networks as Cluster next to each node's local ones", async () => {
    summary = () =>
      Response.json({ ...SWARM_SUMMARY, counts: { ...SWARM_SUMMARY.counts, networks: 5 } });
    renderAt("/env/e1");
    const tip = await showTooltip("Networks");
    // 1 + 1 + 0 local, so 3 of the 5 are swarm-wide.
    expect(tip.textContent).toBe("n1: 1n2: 1n3: 0Cluster: 3");
  });

  it("caps the breakdown at eight nodes and counts the rest", async () => {
    summary = () =>
      Response.json({
        ...SWARM_SUMMARY,
        byNode: Array.from({ length: 10 }, (_, i) => ({
          node: `n${i + 1}`,
          containers: 1,
          images: 1,
          volumes: 0,
          networks: 0,
        })),
        counts: { ...SWARM_SUMMARY.counts, containers: 10 },
      });
    renderAt("/env/e1");
    const tip = await showTooltip("Containers");
    expect(tip.textContent).toContain("n8: 1");
    expect(tip.textContent).not.toContain("n9: 1");
    expect(tip.textContent).toContain("and 2 more nodes");
  });

  it("has no breakdown on a standalone host", async () => {
    environments = () => Response.json({ items: [LAB] });
    summary = () => Response.json(SUMMARY);
    renderAt("/env/e1");
    await waitFor(() => expect(within(tile("Containers")).getByText("7")).toBeTruthy());
    const link = tiles().getByRole("link", { name: /Containers/ });
    fireEvent.focus(link);
    expect(screen.queryByRole("tooltip")).toBeNull();
  });

  it("shows '—' and 'No manager connected' on Nodes and Services while node-local tiles keep working", async () => {
    summary = () =>
      Response.json({
        ...SWARM_SUMMARY,
        counts: { ...SUMMARY.counts },
        environment: {
          ...SWARM_SUMMARY.environment,
          status: { state: "degraded", reason: "not_a_manager" },
        },
      });
    renderAt("/env/e1");
    await waitFor(() => expect(within(tile("Containers")).getByText("7")).toBeTruthy());
    expect(within(tile("Nodes")).getByText("—")).toBeTruthy();
    expect(within(tile("Nodes")).getByText("No manager connected")).toBeTruthy();
    expect(within(tile("Services")).getByText("—")).toBeTruthy();
    expect(within(tile("Services")).getByText("No manager connected")).toBeTruthy();
    // They are not links.
    expect(tiles().queryByRole("link", { name: /Nodes|Services/ })).toBeNull();
    // The node-local tiles still link to their lists.
    expect(
      tiles()
        .getByRole("link", { name: /Containers/ })
        .getAttribute("href"),
    ).toBe("/env/e1/containers");
  });

  it("says the managers have no quorum, not that none is connected, when the quorum is lost", async () => {
    summary = () =>
      Response.json({
        ...SWARM_SUMMARY,
        counts: { ...SUMMARY.counts },
        environment: {
          ...SWARM_SUMMARY.environment,
          status: {
            state: "degraded",
            reason: "no_quorum",
            issues: [{ code: "no_quorum" }],
          },
        },
      });
    renderAt("/env/e1");
    await waitFor(() => expect(within(tile("Nodes")).getByText("—")).toBeTruthy());
    expect(within(tile("Nodes")).getByText("The managers have no quorum")).toBeTruthy();
    expect(screen.queryByText("No manager connected")).toBeNull();
  });

  it("does not show Nodes and Services tiles on a swarm worker, which has the worker notice", async () => {
    summary = () =>
      Response.json({
        ...SWARM_SUMMARY,
        isWorker: true,
        counts: { ...SUMMARY.counts },
        environment: { ...SWARM_SUMMARY.environment, type: "swarm-worker" },
      });
    renderAt("/env/e1");
    await screen.findByText(/This host is a swarm worker/);
    expect(screen.queryByText("Nodes", { selector: "span" })).toBeNull();
    expect(screen.queryByText("No manager connected")).toBeNull();
  });

  it("shows the agents and the agent version on the engine card of a swarm", async () => {
    renderAt("/env/e1");
    const card = (await screen.findByText("Docker engine")).closest(
      "[data-slot=card]",
    ) as HTMLElement;
    await waitFor(() => expect(within(card).getByText("3 of 4 connected")).toBeTruthy());
    expect(within(card).getByText("Agents")).toBeTruthy();
    expect(within(card).getByText("Agent version")).toBeTruthy();
    expect(within(card).getByText("0.2.0")).toBeTruthy();
  });

  it("shows the agent version, without an agent count, for a standalone agent host", async () => {
    environments = () =>
      Response.json({
        items: [{ ...SWARM, type: "standalone", envType: "standalone", agents: null }],
      });
    summary = () =>
      Response.json({
        ...SUMMARY,
        environment: { ...SUMMARY.environment, type: "standalone" },
      });
    renderAt("/env/e1");
    const card = (await screen.findByText("Docker engine")).closest(
      "[data-slot=card]",
    ) as HTMLElement;
    await waitFor(() => expect(within(card).getByText("0.2.0")).toBeTruthy());
    expect(within(card).getByText("Agent version")).toBeTruthy();
    expect(within(card).queryByText("Agents")).toBeNull();
  });

  it("shows neither agent row on the local socket environment", async () => {
    environments = () => Response.json({ items: [{ ...LAB, isLocalSocket: true }] });
    renderAt("/env/e1");
    const card = (await screen.findByText("Docker engine")).closest(
      "[data-slot=card]",
    ) as HTMLElement;
    await waitFor(() => expect(within(card).getByText("29.8.2")).toBeTruthy());
    expect(within(card).queryByText("Agents")).toBeNull();
    expect(within(card).queryByText("Agent version")).toBeNull();
  });

  it("shows each event's node in the feed of a swarm", async () => {
    FakeEventSource.reset();
    vi.stubGlobal("EventSource", FakeEventSource);
    renderAt("/env/e1");
    await screen.findByRole("heading", { name: "Prod swarm" });
    await waitFor(() => expect(feedStream()).toBeTruthy());
    feedStream()?.open();
    feedStream()?.send({
      id: "ev1",
      node: "worker-7",
      time: "2026-10-02T10:00:00Z",
      type: "container",
      action: "start",
      actorId: "abc",
      name: "web-1",
      attributes: {},
    });
    expect(await screen.findByText("worker-7")).toBeTruthy();
  });
});
