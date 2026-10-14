// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { nodeRetry } from "@/api/nodes";
import { queryClient } from "@/api/query-client";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import {
  NODE_ENV,
  NODE_ID,
  type NodeDetailBody,
  type NodeTasksAnswer,
  nodeDetail,
  nodeTask,
} from "@/features/nodes/fixtures";
import { routeTree } from "@/routeTree.gen";
import "@/i18n";
import { makeMe } from "@/test-me";

// The node page (plan 06-14, RES-05, UI-SPEC "Nodes"): the breadcrumb, the header, the four tabs
// and the state of a node that has left the swarm.

const PAGE = `/env/${NODE_ENV}/nodes/${NODE_ID}`;
const BASE = `/api/env/${NODE_ENV}`;

function problem(status: number, type: string) {
  return Response.json({ type, title: "x", status }, { status });
}

interface State {
  sent: string[];
  me: ReturnType<typeof makeMe>;
  detail: () => Response | Promise<Response>;
  tasks: () => Response | Promise<Response>;
  inspect: () => Response | Promise<Response>;
  impact: () => Response | Promise<Response>;
  // What was posted to the node's availability and role routes, with the route.
  posted: { path: string; body: unknown }[];
  post: () => Response | Promise<Response>;
}

let state: State;
const defaults = queryClient.getDefaultOptions();

const tasksAnswer = (extra: Partial<NodeTasksAnswer> = {}): NodeTasksAnswer => ({
  items: [],
  total: 0,
  capped: false,
  ...extra,
});

function node(fetchBody: NodeDetailBody) {
  return Response.json(fetchBody);
}

beforeEach(() => {
  state = {
    sent: [],
    me: makeMe({ envs: { [NODE_ENV]: "viewer" } }),
    detail: () => node(nodeDetail()),
    tasks: () => Response.json(tasksAnswer()),
    inspect: () => Response.json({ document: { ID: NODE_ID, Spec: { Role: "manager" } } }),
    impact: () =>
      Response.json({
        action: "drain",
        hostname: "mgr-1",
        version: 12,
        tasks: [],
        managers: { total: 3, reachable: 3, after: 3 },
        warnings: [],
        needsConfirm: false,
      }),
    posted: [],
    post: () => new Response(null, { status: 204 }),
  };
  queryClient.setDefaultOptions({ queries: { retry: false, refetchOnWindowFocus: false } });
  nodeRetry.attempts = 0;
  setCsrfToken(null);
  FakeEventSource.reset();
  vi.stubGlobal("EventSource", FakeEventSource);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request): Promise<Response> => {
      const url = new URL(req.url);
      state.sent.push(url.pathname);
      if (req.method === "POST") {
        state.posted.push({ path: url.pathname, body: await req.json() });
        return state.post();
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
                id: NODE_ENV,
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
              id: NODE_ENV,
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
        case `${BASE}/nodes/${NODE_ID}`:
          return state.detail();
        case `${BASE}/nodes/${NODE_ID}/tasks`:
          return state.tasks();
        case `${BASE}/nodes/${NODE_ID}/inspect`:
          return state.inspect();
        case `${BASE}/nodes/${NODE_ID}/impact`:
          return state.impact();
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
  nodeRetry.attempts = 1;
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

// headerOf is the page's header, so a word the cards repeat is not counted twice.
function headerOf(): HTMLElement {
  const header = screen.getByRole("heading", { level: 2 }).closest("header");
  if (!header) throw new Error("no header");
  return header;
}

describe("the page frame", () => {
  it("shows the breadcrumb Nodes / name, the name with its status and badges, and the address and Docker version", async () => {
    renderAt();
    expect(await screen.findByRole("heading", { name: "mgr-1" })).toBeTruthy();
    const crumb = screen.getByRole("navigation", { name: "Breadcrumb" });
    expect(within(crumb).getByRole("link", { name: "Nodes" }).getAttribute("href")).toBe(
      "/env/e1/nodes",
    );
    expect(crumb.textContent).toContain("mgr-1");
    const header = headerOf();
    expect(within(header).getByText("Ready")).toBeTruthy();
    expect(within(header).getByText("Manager")).toBeTruthy();
    expect(within(header).getByText("Leader")).toBeTruthy();
    expect(within(header).getByText("Runs Bayplan")).toBeTruthy();
    expect(within(header).getByText("10.0.0.1 · Docker 29.1.0")).toBeTruthy();
    expect(document.title).toBe("mgr-1 · Lab");
  });

  it("shows a worker as Worker, with no Leader and no Runs Bayplan badge", async () => {
    state.detail = () =>
      node(nodeDetail({ role: "worker", leader: false, runsBayplan: false, reachability: "" }));
    renderAt();
    await screen.findByRole("heading", { name: "mgr-1" });
    const header = headerOf();
    expect(within(header).getByText("Worker")).toBeTruthy();
    expect(within(header).queryByText("Leader")).toBeNull();
    expect(within(header).queryByText("Runs Bayplan")).toBeNull();
  });

  it("shows a node that is down with the offline word", async () => {
    state.detail = () => node(nodeDetail({ state: "down" }));
    renderAt();
    await screen.findByRole("heading", { name: "mgr-1" });
    expect(within(headerOf()).getByText("Down")).toBeTruthy();
  });

  it("asks for the node by the ID in the address", async () => {
    renderAt();
    await screen.findByRole("heading", { name: "mgr-1" });
    expect(state.sent).toContain(`${BASE}/nodes/${NODE_ID}`);
  });

  it("has the tabs Overview, Tasks, Labels and Inspect, the first at the page's own address", async () => {
    renderAt();
    const overview = await screen.findByRole("tab", { name: "Overview" });
    expect(overview.getAttribute("href")).toBe(PAGE);
    await waitFor(() => expect(overview.getAttribute("data-active")).toBeTruthy());
    expect(screen.getAllByRole("tab").map((tab) => tab.textContent)).toEqual([
      "Overview",
      "Tasks",
      "Labels",
      "Inspect",
    ]);
    expect(screen.getByRole("tab", { name: "Tasks" }).getAttribute("href")).toBe(`${PAGE}/tasks`);
    expect(screen.getByRole("tab", { name: "Labels" }).getAttribute("href")).toBe(`${PAGE}/labels`);
    expect(screen.getByRole("tab", { name: "Inspect" }).getAttribute("href")).toBe(
      `${PAGE}/inspect`,
    );
  });

  it("shows the tabs and skeletons at once, before the node has loaded", async () => {
    state.detail = () => new Promise<Response>(() => {});
    renderAt();
    expect(await screen.findByRole("tab", { name: "Overview" })).toBeTruthy();
    expect(screen.getAllByRole("status").some((s) => s.textContent === "Loading the node")).toBe(
      true,
    );
    expect(screen.queryByRole("heading", { name: "mgr-1" })).toBeNull();
  });

  it("writes a host name as text, whatever it holds", async () => {
    state.detail = () => node(nodeDetail({ hostname: "<img src=x onerror=alert(1)>" }));
    renderAt();
    expect(
      await screen.findByRole("heading", { name: "<img src=x onerror=alert(1)>" }),
    ).toBeTruthy();
    expect(document.querySelector("img")).toBeNull();
  });

  it("shows no permission to a user who cannot read nodes", async () => {
    state.me = makeMe({});
    renderAt();
    expect(await screen.findByText("You don't have permission to view this page")).toBeTruthy();
    expect(state.sent).not.toContain(`${BASE}/nodes/${NODE_ID}`);
  });
});

describe("the states of the page", () => {
  it("turns into This node is gone, with Back to nodes, when the swarm no longer has it", async () => {
    state.detail = () => problem(404, "node_not_found");
    renderAt();
    expect(await screen.findByRole("heading", { name: "This node is gone" })).toBeTruthy();
    expect(
      screen.getByText("It left the swarm or was removed. Go back to the list to find it."),
    ).toBeTruthy();
    expect(screen.getByRole("link", { name: "Back to nodes" }).getAttribute("href")).toBe(
      "/env/e1/nodes",
    );
    // There is nothing left for the tabs to show.
    expect(screen.queryByRole("tab", { name: "Overview" })).toBeNull();
    expect(document.title).toBe("This node is gone · Lab");
  });

  it("turns into the gone state when the node leaves the swarm while its page is open", async () => {
    renderAt();
    await screen.findByRole("heading", { name: "mgr-1" });
    state.detail = () => problem(404, "node_not_found");
    await act(async () => {
      await queryClient.invalidateQueries({ queryKey: ["env", NODE_ENV, "nodes"] });
    });
    expect(await screen.findByRole("heading", { name: "This node is gone" })).toBeTruthy();
  });

  it("shows a load error with a retry that names the node", async () => {
    state.detail = () => problem(500, "internal");
    renderAt();
    expect(await screen.findByText("Couldn't load n1.")).toBeTruthy();
    state.detail = () => node(nodeDetail());
    fireEvent.click(screen.getByRole("button", { name: "Retry loading n1" }));
    expect(await screen.findByRole("heading", { name: "mgr-1" })).toBeTruthy();
  });
});

describe("the Overview tab", () => {
  it("shows the Node card with the ID, host name, address, role, status, availability and times", async () => {
    renderAt();
    await screen.findByRole("heading", { name: "mgr-1" });
    const node = within(await screen.findByRole("region", { name: "Node" }));
    for (const key of [
      "ID",
      "Hostname",
      "Address",
      "Role",
      "Status",
      "Availability",
      "Created",
      "Updated",
    ]) {
      expect(node.getByRole("cell", { name: key })).toBeTruthy();
    }
    expect(node.getByText("n1")).toBeTruthy();
    expect(node.getByText("mgr-1")).toBeTruthy();
    expect(node.getByText("10.0.0.1")).toBeTruthy();
    expect(node.getByText("Manager")).toBeTruthy();
    expect(node.getByText("Ready")).toBeTruthy();
    expect(node.getByText("Active")).toBeTruthy();
  });

  it("shows the System card with the engine, operating system, architecture, CPUs and memory", async () => {
    renderAt();
    const system = within(await screen.findByRole("region", { name: "System" }));
    expect(system.getByText("29.1.0")).toBeTruthy();
    expect(system.getByText("linux")).toBeTruthy();
    expect(system.getByText("arm64")).toBeTruthy();
    expect(system.getByText("8")).toBeTruthy();
    expect(system.getByText("31 GiB")).toBeTruthy();
  });

  it("shows the Manager card for a manager only: reachability, whether it leads, and its address", async () => {
    renderAt();
    const manager = within(await screen.findByRole("region", { name: "Manager" }));
    expect(manager.getByText("Reachable")).toBeTruthy();
    expect(manager.getByText("Yes")).toBeTruthy();
    expect(manager.getByText("10.0.0.1:2377")).toBeTruthy();
    cleanup();
    queryClient.clear();
    state.detail = () =>
      node(nodeDetail({ role: "worker", leader: false, reachability: "", managerAddr: "" }));
    renderAt();
    await screen.findByRole("region", { name: "Node" });
    expect(screen.queryByRole("region", { name: "Manager" })).toBeNull();
  });

  it("shows the Bayplan agent card with Reporting, the version and when it was last seen", async () => {
    renderAt();
    const agent = within(await screen.findByRole("region", { name: "Bayplan agent" }));
    expect(agent.getByRole("cell", { name: "Reporting" })).toBeTruthy();
    expect(agent.getByText("Yes")).toBeTruthy();
    expect(agent.getByText("0.1.0")).toBeTruthy();
    expect(agent.getByRole("cell", { name: "Last seen" })).toBeTruthy();
  });

  it("says No for Reporting, with dashes, for a node whose agent is not connected", async () => {
    state.detail = () =>
      node(nodeDetail({ agent: { reporting: false, version: "", lastSeen: "" } }));
    renderAt();
    const agent = within(await screen.findByRole("region", { name: "Bayplan agent" }));
    expect(agent.getByText("No")).toBeTruthy();
    expect(agent.getAllByText("—").length).toBe(2);
  });

  it("shows the engine labels read-only, with the reason they cannot be changed here", async () => {
    renderAt();
    const labels = within(await screen.findByRole("region", { name: "Engine labels" }));
    expect(labels.getByText("provider")).toBeTruthy();
    expect(labels.getByText("homelab")).toBeTruthy();
    expect(
      labels.getByText("Set in the node's Docker daemon settings. Bayplan can't change them."),
    ).toBeTruthy();
  });

  it("shows dashes for the platform and resources a node does not report", async () => {
    state.detail = () => node(nodeDetail({ os: "", arch: "", cpus: 0, memoryBytes: 0 }));
    renderAt();
    const system = within(await screen.findByRole("region", { name: "System" }));
    expect(system.getAllByText("—")).toHaveLength(4);
  });

  it("writes engine labels as text, whatever they hold", async () => {
    state.detail = () => node(nodeDetail({ engineLabels: { "<b>k</b>": "<img src=x>" } }));
    renderAt();
    await screen.findByRole("region", { name: "Engine labels" });
    expect(document.querySelector("img")).toBeNull();
    expect(document.querySelector("b")).toBeNull();
  });
});

describe("the Tasks tab", () => {
  it("lists the tasks with Task, Service as a link, State, Desired, Updated and Error", async () => {
    state.tasks = () =>
      Response.json(
        tasksAnswer({
          total: 2,
          items: [
            nodeTask(),
            nodeTask({
              id: "task2abcdef12345678",
              service: "api",
              serviceId: "svc-api",
              slot: 1,
              state: "failed",
              dockerState: "failed",
              desiredState: "shutdown",
              error: "task: non-zero exit (1)",
            }),
          ],
        }),
      );
    renderAt(`${PAGE}/tasks`);
    // The skeleton is a table of the same name, so wait for the rows.
    await screen.findByRole("link", { name: "web" });
    const table = screen.getByRole("table", { name: "Tasks" });
    for (const header of ["Task", "Service", "State", "Desired", "Updated", "Error"]) {
      expect(within(table).getByRole("columnheader", { name: header })).toBeTruthy();
    }
    // The Node column is the page itself, so it is not there.
    expect(within(table).queryByRole("columnheader", { name: "Node" })).toBeNull();
    const web = within(table).getByRole("link", { name: "web" });
    expect(web.getAttribute("href")).toBe("/env/e1/services/svc-web");
    const row = web.closest("tr") as HTMLElement;
    expect(within(row).getByText("web.2")).toBeTruthy();
    expect(within(row).getByText("task1abcdef1")).toBeTruthy();
    expect(within(row).getAllByText("Running").length).toBe(2);
    const failed = within(table).getByRole("link", { name: "api" }).closest("tr") as HTMLElement;
    expect(within(failed).getByText("Failed")).toBeTruthy();
    expect(within(failed).getByText("Shut down")).toBeTruthy();
    expect(within(failed).getByText("task: non-zero exit (1)")).toBeTruthy();
    expect(screen.getByText("2 tasks")).toBeTruthy();
  });

  it("says No tasks on this node when it has none", async () => {
    renderAt(`${PAGE}/tasks`);
    expect(await screen.findByRole("heading", { name: "No tasks on this node" })).toBeTruthy();
  });

  it("says earlier tasks are left out when the answer is capped", async () => {
    state.tasks = () =>
      Response.json(tasksAnswer({ items: [nodeTask()], total: 90, capped: true }));
    renderAt(`${PAGE}/tasks`);
    expect(
      await screen.findByText("Older tasks are left out. The swarm holds 90 for this node."),
    ).toBeTruthy();
  });

  it("shows a skeleton while the tasks load, and its own error with a retry", async () => {
    state.tasks = () => new Promise<Response>(() => {});
    renderAt(`${PAGE}/tasks`);
    expect(await screen.findByText("Loading tasks")).toBeTruthy();
    cleanup();
    queryClient.clear();
    state.tasks = () => problem(500, "internal");
    renderAt(`${PAGE}/tasks`);
    expect(await screen.findByText("Couldn't load the tasks of this node.")).toBeTruthy();
    state.tasks = () => Response.json(tasksAnswer({ items: [nodeTask()], total: 1 }));
    fireEvent.click(screen.getByRole("button", { name: "Retry loading tasks" }));
    expect(await screen.findByRole("link", { name: "web" })).toBeTruthy();
  });

  it("writes a task's error and a service name as text, whatever they hold", async () => {
    state.tasks = () =>
      Response.json(
        tasksAnswer({
          items: [
            nodeTask({
              service: "<img src=x>",
              error: "<b>boom</b>",
              state: "failed",
              dockerState: "failed",
            }),
          ],
          total: 1,
        }),
      );
    renderAt(`${PAGE}/tasks`);
    expect(await screen.findByText("<b>boom</b>")).toBeTruthy();
    expect(document.querySelector("img")).toBeNull();
    expect(document.querySelector("b")).toBeNull();
  });
});

describe("the Labels tab", () => {
  it("shows the node's labels read-only, by key", async () => {
    renderAt(`${PAGE}/labels`);
    expect(await screen.findByRole("heading", { name: "Node labels" })).toBeTruthy();
    const table = screen.getByRole("table", { name: "Node labels" });
    expect(
      within(table)
        .getAllByRole("row")
        .map((r) => r.textContent),
    ).toEqual(["diskssd", "zonea"]);
    expect(
      screen.getByText(
        "Services use labels in placement constraints, such as node.labels.disk==ssd.",
      ),
    ).toBeTruthy();
    // Editing comes with a later plan: nothing here changes a label.
    expect(screen.queryByRole("button", { name: /label/i })).toBeNull();
  });

  it("says No labels. when the node has none", async () => {
    state.detail = () => node(nodeDetail({ labels: {} }));
    renderAt(`${PAGE}/labels`);
    expect(await screen.findByText("No labels.")).toBeTruthy();
  });
});

describe("the Inspect tab", () => {
  it("shows the node's raw document", async () => {
    renderAt(`${PAGE}/inspect`);
    expect(await screen.findByText(/"Role": "manager"/)).toBeTruthy();
    expect(state.sent).toContain(`${BASE}/nodes/${NODE_ID}/inspect`);
  });

  it("shows its own load error with a retry", async () => {
    state.inspect = () => problem(500, "internal");
    renderAt(`${PAGE}/inspect`);
    expect(await screen.findByText("Couldn't load the inspect document of mgr-1.")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Retry loading the inspect document" })).toBeTruthy();
  });
});

describe("the node's actions", () => {
  beforeEach(() => {
    state.me = makeMe({ envs: { [NODE_ENV]: "admin" } });
  });

  const names = (el: HTMLElement) =>
    within(el)
      .getAllByRole("button")
      .map((b) => b.textContent);

  async function openMore() {
    const more = await screen.findByRole("button", { name: "More" });
    await act(async () => {
      fireEvent.keyDown(more, { key: "Enter" });
    });
    return screen.findByRole("menu");
  }

  it("offers an active manager Drain… and Pause…, and Demote… and Copy node ID under More", async () => {
    renderAt();
    await screen.findByRole("heading", { name: "mgr-1" });
    expect(names(headerOf())).toEqual(["Drain…", "Pause…", "More"]);
    const menu = await openMore();
    expect(
      within(menu)
        .getAllByRole("menuitem")
        .map((i) => i.textContent),
    ).toEqual(["Demote…", "Copy node ID"]);
  });

  it("offers a paused worker Drain… and Activate node, and Promote… under More", async () => {
    state.detail = () =>
      node(
        nodeDetail({
          role: "worker",
          leader: false,
          runsBayplan: false,
          reachability: "",
          availability: "pause",
        }),
      );
    renderAt();
    await screen.findByRole("heading", { name: "mgr-1" });
    expect(names(headerOf())).toEqual(["Drain…", "Activate node", "More"]);
    const menu = await openMore();
    expect(
      within(menu)
        .getAllByRole("menuitem")
        .map((i) => i.textContent),
    ).toEqual(["Promote…", "Copy node ID"]);
  });

  it("offers a drained node Activate node only", async () => {
    state.detail = () => node(nodeDetail({ availability: "drain" }));
    renderAt();
    await screen.findByRole("heading", { name: "mgr-1" });
    expect(names(headerOf())).toEqual(["Activate node", "More"]);
  });

  it("opens the drain dialog, sends the drain and shows Draining… until the page shows it", async () => {
    renderAt();
    await screen.findByRole("heading", { name: "mgr-1" });
    fireEvent.click(within(headerOf()).getByRole("button", { name: "Drain…" }));
    const dialog = await screen.findByRole("dialog", { name: "Drain mgr-1?" });
    await waitFor(() =>
      expect(within(dialog).queryByText("Checking what this will do…")).toBeNull(),
    );
    fireEvent.click(within(dialog).getByRole("button", { name: "Drain node" }));
    await waitFor(() => expect(state.posted).toHaveLength(1));
    expect(state.posted[0]).toEqual({
      path: `${BASE}/nodes/${NODE_ID}/availability`,
      body: { availability: "drain", version: 12 },
    });
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(within(headerOf()).getByText("Draining…")).toBeTruthy();
  });

  it("activates a node with no dialog", async () => {
    state.detail = () => node(nodeDetail({ availability: "drain" }));
    renderAt();
    await screen.findByRole("heading", { name: "mgr-1" });
    fireEvent.click(within(headerOf()).getByRole("button", { name: "Activate node" }));
    expect(await screen.findByText("Activated mgr-1.")).toBeTruthy();
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(state.posted[0]).toEqual({
      path: `${BASE}/nodes/${NODE_ID}/availability`,
      body: { availability: "active", version: 12 },
    });
  });

  it("copies the node's ID", async () => {
    const writeText = vi.fn(async () => {});
    vi.stubGlobal("navigator", { ...navigator, clipboard: { writeText } });
    renderAt();
    await screen.findByRole("heading", { name: "mgr-1" });
    const menu = await openMore();
    fireEvent.click(within(menu).getByRole("menuitem", { name: "Copy node ID" }));
    await waitFor(() => expect(writeText).toHaveBeenCalledWith(NODE_ID));
  });

  it("shows a Viewer the changes unavailable with Needs Admin", async () => {
    state.me = makeMe({ envs: { [NODE_ENV]: "viewer" } });
    renderAt();
    await screen.findByRole("heading", { name: "mgr-1" });
    const drain = within(headerOf()).getByRole("button", { name: "Drain…" });
    expect(drain.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(drain);
    expect(await screen.findAllByText("Needs Admin")).toBeTruthy();
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(state.posted).toEqual([]);
    // Copying the ID is a read, open to a Viewer.
    const menu = await openMore();
    expect(
      within(menu).getByRole("menuitem", { name: "Copy node ID" }).getAttribute("aria-disabled"),
    ).toBeNull();
  });
});
