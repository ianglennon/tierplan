// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { components } from "@/api/schema";
import { TooltipProvider } from "@/components/ui/tooltip";
import { NodeList } from "@/features/nodes/node-list";
import {
  type NodeSearch,
  type NodeSearchPatch,
  nodeSearchSchema,
  nodeStatusList,
  nodeStatusParam,
} from "@/features/nodes/node-search";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// The Nodes page (plan 06-14, RES-05, UI-SPEC "Nodes"): the swarm's nodes with their role, health,
// agent and task counts, found by a search and the status chips the server answers.

const ENV = "e1";

type NodeItem = components["schemas"]["NodeItem"];

const node = (extra: Partial<NodeItem> = {}): NodeItem => ({
  id: "n1",
  hostname: "mgr-1",
  role: "manager",
  leader: true,
  runsBayplan: true,
  state: "ready",
  availability: "active",
  reachability: "reachable",
  address: "10.0.0.1",
  engineVersion: "29.1.0",
  os: "linux",
  arch: "aarch64",
  cpus: 8,
  memoryBytes: 31 * 1024 ** 3,
  tasksRunning: 3,
  tasksNotRunning: 1,
  agentReporting: true,
  targetNodeId: "t1",
  ...extra,
});

const NODES: NodeItem[] = [
  node(),
  node({
    id: "n2",
    hostname: "mgr-2",
    leader: false,
    runsBayplan: false,
    state: "down",
    reachability: "unreachable",
    address: "10.0.0.2",
    tasksRunning: 0,
    tasksNotRunning: 0,
    agentReporting: false,
    targetNodeId: "",
  }),
  node({
    id: "n3",
    hostname: "wkr-1",
    role: "worker",
    leader: false,
    runsBayplan: false,
    availability: "drain",
    reachability: "",
    address: "10.0.0.3",
    os: "",
    arch: "",
    cpus: 0,
    memoryBytes: 0,
    tasksRunning: 2,
    tasksNotRunning: 0,
    agentReporting: false,
    targetNodeId: "",
  }),
  node({
    id: "n4",
    hostname: "wkr-2",
    role: "worker",
    leader: false,
    runsBayplan: false,
    availability: "pause",
    reachability: "",
    address: "10.0.0.4",
    arch: "x86_64",
    cpus: 0.5,
    tasksRunning: 7,
    tasksNotRunning: 0,
    targetNodeId: "t4",
  }),
];

interface Sent {
  method: string;
  path: string;
  search: string;
}

let sent: Sent[];
let items: NodeItem[];
let noManager: boolean;
let tasksKnown: boolean;
let answerList: ((url: URL) => Response | Promise<Response>) | undefined;
let changes: NodeSearchPatch[];
let me: ReturnType<typeof makeMe>;
let client: QueryClient;

function listBody(rows: NodeItem[], extra: Record<string, unknown> = {}) {
  return {
    items: rows,
    total: rows.length,
    unfilteredTotal: rows.length,
    counts: {
      managers: rows.filter((n) => n.role === "manager").length,
      workers: rows.filter((n) => n.role === "worker").length,
      drained: rows.filter((n) => n.availability === "drain").length,
      down: rows.filter((n) => n.state === "down" || n.state === "disconnected").length,
    },
    noManager,
    tasksKnown,
    fetchedAt: "2026-10-14T09:00:00Z",
    ...extra,
  };
}

beforeEach(() => {
  sent = [];
  items = NODES;
  noManager = false;
  tasksKnown = true;
  answerList = undefined;
  changes = [];
  me = makeMe({ envs: { [ENV]: "viewer" } });
  client = new QueryClient({ defaultOptions: { queries: { retry: false, retryDelay: 1 } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      sent.push({ method: req.method, path: url.pathname, search: url.search });
      if (url.pathname === "/api/me") return Response.json(me);
      if (url.pathname === `/api/env/${ENV}/nodes` && req.method === "GET") {
        return answerList ? answerList(url) : Response.json(listBody(items));
      }
      if (url.pathname === "/api/environments") {
        return Response.json({ items: [{ id: ENV, name: "Swarm", envType: "swarm" }] });
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  client.clear();
});

function mount(search: NodeSearch = {}) {
  return render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <NodeList envId={ENV} search={search} onSearchChange={(p) => changes.push(p)} />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

// rowOf is the table row of a node, found by its name's link.
function rowOf(name: string): HTMLElement {
  const row = screen.getByRole("link", { name }).closest("tr");
  if (!row) throw new Error(`no row for ${name}`);
  return row;
}

const listRequests = () =>
  sent.filter((s) => s.method === "GET" && s.path === `/api/env/${ENV}/nodes`).map((s) => s.search);

describe("the node search", () => {
  it("reads a comma list of statuses into the known ones, once each, in chip order", () => {
    expect(nodeStatusList("down,managers, nope ,managers")).toEqual(["managers", "down"]);
    expect(nodeStatusList(undefined)).toEqual([]);
    expect(nodeStatusList("")).toEqual([]);
  });

  it("writes the statuses back in chip order, or nothing when none is chosen", () => {
    expect(nodeStatusParam(["down", "workers"])).toBe("workers,down");
    expect(nodeStatusParam([])).toBeUndefined();
  });

  it("keeps q, status, sort and dir, and drops what does not fit", () => {
    expect(
      nodeSearchSchema.parse({ q: "mgr", status: "down,x,managers", sort: "tasks", dir: "desc" }),
    ).toEqual({
      q: "mgr",
      status: "managers,down",
      sort: "tasks",
      dir: "desc",
    });
    expect(nodeSearchSchema.parse({ q: "x".repeat(129), sort: "nope", dir: "up" })).toEqual({});
    expect(nodeSearchSchema.parse({ status: "nope" })).toEqual({});
  });
});

describe("NodeList", () => {
  it("shows the columns of the UI-SPEC", async () => {
    mount();
    await screen.findByRole("link", { name: "mgr-1" });
    for (const header of [
      "Name",
      "Status",
      "Availability",
      "Role",
      "Address",
      "Engine",
      "Platform",
      "CPU",
      "Memory",
      "Tasks",
      "Agent",
    ]) {
      expect(screen.getByRole("columnheader", { name: header })).toBeTruthy();
    }
  });

  it("shows a manager's name with Leader and Runs Bayplan, its status, role, address, engine, platform, resources and agent", async () => {
    mount();
    await screen.findByRole("link", { name: "mgr-1" });
    const row = rowOf("mgr-1");
    expect(within(row).getByText("Leader")).toBeTruthy();
    expect(within(row).getByText("Runs Bayplan")).toBeTruthy();
    expect(within(row).getByText("Ready")).toBeTruthy();
    expect(within(row).getByText("Active")).toBeTruthy();
    expect(within(row).getByText("Manager")).toBeTruthy();
    expect(within(row).getByText("10.0.0.1")).toBeTruthy();
    expect(within(row).getByText("29.1.0")).toBeTruthy();
    expect(within(row).getByText("linux/arm64")).toBeTruthy();
    expect(within(row).getByText("8")).toBeTruthy();
    expect(within(row).getByText("31 GiB")).toBeTruthy();
    expect(within(row).getByText("3")).toBeTruthy();
    expect(within(row).getByText("Reporting")).toBeTruthy();
  });

  it("links the name to the node's page", async () => {
    mount();
    const link = await screen.findByRole("link", { name: "mgr-1" });
    expect(link.getAttribute("href")).toBe("/env/e1/nodes/n1");
    expect(screen.getByRole("link", { name: "wkr-2" }).getAttribute("href")).toBe(
      "/env/e1/nodes/n4",
    );
  });

  it("marks an unreachable manager with an offline dot and Unreachable, and a node with no agent as Not reporting", async () => {
    mount();
    await screen.findByRole("link", { name: "mgr-2" });
    const row = rowOf("mgr-2");
    expect(within(row).getByText("Manager")).toBeTruthy();
    expect(within(row).getByText("Unreachable")).toBeTruthy();
    expect(within(row).getByText("Down")).toBeTruthy();
    expect(within(row).getByText("Not reporting")).toBeTruthy();
    // A reachable manager and a worker say nothing of reachability.
    expect(within(rowOf("mgr-1")).queryByText("Unreachable")).toBeNull();
    expect(within(rowOf("wkr-1")).queryByText("Unreachable")).toBeNull();
  });

  it("keeps a row whose agent is not reporting, and shows a dash for platform, CPU and memory it does not know", async () => {
    mount();
    await screen.findByRole("link", { name: "wkr-1" });
    const row = rowOf("wkr-1");
    expect(within(row).getByText("Not reporting")).toBeTruthy();
    expect(within(row).getByText("Worker")).toBeTruthy();
    // Platform, CPU and memory.
    expect(within(row).getAllByText("—")).toHaveLength(3);
  });

  it("writes availability in words: Paused, Draining with its tasks left, Drained when none are", async () => {
    mount();
    await screen.findByRole("link", { name: "wkr-1" });
    expect(within(rowOf("wkr-2")).getByText("Paused")).toBeTruthy();
    expect(within(rowOf("wkr-1")).getByText("Draining (2 tasks left)")).toBeTruthy();
    cleanup();
    items = [node({ availability: "drain", tasksRunning: 0, tasksNotRunning: 0 })];
    mount();
    await screen.findByRole("link", { name: "mgr-1" });
    expect(within(rowOf("mgr-1")).getByText("Drained")).toBeTruthy();
  });

  it("maps the engine's architecture names to Docker's platform names and keeps a fractional CPU", async () => {
    mount();
    await screen.findByRole("link", { name: "wkr-2" });
    const row = rowOf("wkr-2");
    expect(within(row).getByText("linux/amd64")).toBeTruthy();
    expect(within(row).getByText("0.5")).toBeTruthy();
  });

  it("gives the running tasks with a tooltip of how many are not running, and a dash when the tasks could not be read", async () => {
    mount();
    await screen.findByRole("link", { name: "mgr-1" });
    const count = within(rowOf("mgr-1")).getByText("3");
    fireEvent.focus(count);
    expect((await screen.findAllByText("3 running, 1 not running")).length).toBeGreaterThan(0);
    cleanup();
    tasksKnown = false;
    mount();
    await screen.findByRole("link", { name: "mgr-1" });
    expect(within(rowOf("mgr-1")).queryByText("3")).toBeNull();
    // Tasks, and the platform and CPU a worker does not report, are dashes; the manager's own row has one.
    expect(within(rowOf("mgr-1")).getAllByText("—")).toHaveLength(1);
  });

  it("lists the nodes in the order the server sent, by host name then ID", async () => {
    mount();
    await screen.findByRole("link", { name: "mgr-1" });
    const names = screen
      .getAllByRole("link")
      .map((l) => l.textContent)
      .filter((n) => /^(mgr|wkr)-/.test(n ?? ""));
    expect(names).toEqual(["mgr-1", "mgr-2", "wkr-1", "wkr-2"]);
  });

  it("sorts by the column chosen, and says which way", async () => {
    mount({ sort: "tasks", dir: "desc" });
    await screen.findByRole("link", { name: "mgr-1" });
    const names = screen
      .getAllByRole("link")
      .map((l) => l.textContent)
      .filter((n) => /^(mgr|wkr)-/.test(n ?? ""));
    expect(names).toEqual(["wkr-2", "mgr-1", "wkr-1", "mgr-2"]);
    expect(screen.getByRole("columnheader", { name: /Tasks/ }).getAttribute("aria-sort")).toBe(
      "descending",
    );
  });

  it("asks to sort by a column when its header is pressed", async () => {
    mount();
    await screen.findByRole("link", { name: "mgr-1" });
    fireEvent.click(within(screen.getByRole("columnheader", { name: /Role/ })).getByRole("button"));
    expect(changes).toEqual([{ sort: "role", dir: "asc" }]);
  });

  it("says how many nodes and managers there are, plural-aware", async () => {
    mount();
    expect(await screen.findByText("4 nodes, 2 managers")).toBeTruthy();
    cleanup();
    items = [node()];
    mount();
    expect(await screen.findByText("1 node, 1 manager")).toBeTruthy();
    cleanup();
    items = [node({ role: "worker", leader: false })];
    mount();
    expect(await screen.findByText("1 node, 0 managers")).toBeTruthy();
  });

  it("says how many of the nodes match when a search or chip is set", async () => {
    answerList = () =>
      Response.json(
        listBody([NODES[0] as NodeItem], {
          total: 1,
          unfilteredTotal: 4,
          counts: { managers: 2, workers: 2, drained: 1, down: 1 },
        }),
      );
    mount({ status: "managers" });
    expect(await screen.findByText("1 of 4 nodes")).toBeTruthy();
  });
});

describe("the nodes list: search and chips", () => {
  it("asks the server for the search and the statuses", async () => {
    mount({ q: "mgr", status: "managers,down" });
    await screen.findByRole("link", { name: "mgr-1" });
    const asked = listRequests().at(0) ?? "";
    expect(new URLSearchParams(asked).get("q")).toBe("mgr");
    expect(new URLSearchParams(asked).get("status")).toBe("managers,down");
  });

  it("asks for the whole list with no search and no chip", async () => {
    mount();
    await screen.findByRole("link", { name: "mgr-1" });
    expect(listRequests().at(0)).toBe("");
  });

  it("shows Managers, Workers, Drained and Down with the counts of the whole swarm, in a group named Filter nodes", async () => {
    mount();
    await screen.findByRole("link", { name: "mgr-1" });
    const group = screen.getByRole("group", { name: "Filter nodes" });
    expect(within(group).getByRole("button", { name: "Managers 2" })).toBeTruthy();
    expect(within(group).getByRole("button", { name: "Workers 2" })).toBeTruthy();
    expect(within(group).getByRole("button", { name: "Drained 1" })).toBeTruthy();
    expect(within(group).getByRole("button", { name: "Down 1" })).toBeTruthy();
  });

  it("toggles a chip through the search, in chip order, and shows it pressed", async () => {
    mount({ status: "down" });
    await screen.findByRole("link", { name: "mgr-1" });
    expect(screen.getByRole("button", { name: "Down 1" }).getAttribute("aria-pressed")).toBe(
      "true",
    );
    fireEvent.click(screen.getByRole("button", { name: "Managers 2" }));
    expect(changes).toEqual([{ status: "managers,down" }]);
  });

  it("commits a search through the search box", async () => {
    mount();
    const box = await screen.findByRole("searchbox", { name: "Search nodes" });
    expect(box.getAttribute("placeholder")).toBe("Search by name, address or label");
    fireEvent.change(box, { target: { value: "wkr" } });
    // The box waits for the typing to stop before it changes the URL.
    await waitFor(() => expect(changes).toEqual([{ q: "wkr" }]));
  });

  it("shows Clear filters only while a search or a chip is set, and clears both", async () => {
    mount();
    await screen.findByRole("link", { name: "mgr-1" });
    expect(screen.queryByRole("button", { name: "Clear filters" })).toBeNull();
    cleanup();
    mount({ q: "mgr", status: "down" });
    await screen.findByRole("link", { name: "mgr-1" });
    fireEvent.click(screen.getByRole("button", { name: "Clear filters" }));
    expect(changes).toEqual([{ q: undefined, status: undefined }]);
  });
});

describe("the nodes list: states", () => {
  it("shows 8 skeleton rows and the toolbar at once while loading", async () => {
    answerList = () => new Promise<Response>(() => {});
    mount();
    expect(await screen.findByRole("searchbox", { name: "Search nodes" })).toBeTruthy();
    expect(screen.getByRole("group", { name: "Filter nodes" })).toBeTruthy();
    expect(screen.getAllByRole("row")).toHaveLength(9);
    expect(screen.getByRole("table").getAttribute("aria-busy")).toBe("true");
  });

  it("shows the list error with Retry loading nodes, and loads again on press", async () => {
    answerList = () => Response.json({ title: "Bad", status: 500 }, { status: 500 });
    mount();
    expect(await screen.findByText("Couldn't load nodes.")).toBeTruthy();
    expect(screen.getByText("Docker may be unreachable. Try again.")).toBeTruthy();
    answerList = undefined;
    fireEvent.click(screen.getByRole("button", { name: "Retry loading nodes" }));
    expect(await screen.findByRole("link", { name: "mgr-1" })).toBeTruthy();
    expect(screen.queryByText("Couldn't load nodes.")).toBeNull();
  });

  it("says no manager is connected above the rows last read", async () => {
    noManager = true;
    mount();
    expect(await screen.findByText("No manager is connected")).toBeTruthy();
    expect(screen.getByRole("link", { name: "mgr-1" })).toBeTruthy();
  });

  it("says nothing to show when the swarm answers no nodes", async () => {
    items = [];
    mount();
    expect(await screen.findByRole("heading", { name: "No nodes" })).toBeTruthy();
    expect(screen.getByText("Nothing to show on Swarm right now.")).toBeTruthy();
  });

  it("says no nodes match these filters, with Clear filters, when a search or a chip is set", async () => {
    items = [];
    mount({ q: "zzz" });
    const heading = await screen.findByRole("heading", { name: "No nodes match these filters." });
    const empty = heading.closest("[data-slot=empty]") as HTMLElement;
    fireEvent.click(within(empty).getByRole("button", { name: "Clear filters" }));
    expect(changes).toEqual([{ q: undefined, status: undefined }]);
  });

  it("writes the search as text, whatever it holds", async () => {
    items = [];
    mount({ q: "<img src=x onerror=alert(1)>" });
    expect(await screen.findByRole("heading", { name: /^No nodes match/ })).toBeTruthy();
    expect(document.querySelector("img")).toBeNull();
  });

  it("says a standalone host is not a swarm", async () => {
    answerList = () =>
      Response.json({ type: "not_a_swarm", title: "x", status: 409 }, { status: 409 });
    mount();
    expect(await screen.findByText("This isn't a swarm")).toBeTruthy();
  });

  it("shows no permission to a user who cannot read nodes", async () => {
    me = makeMe({});
    mount();
    expect(await screen.findByText("You don't have permission to view this page")).toBeTruthy();
    expect(listRequests()).toHaveLength(0);
  });

  it("writes a hostname and an address as text, whatever they hold", async () => {
    items = [node({ hostname: "<img src=x onerror=alert(1)>", address: "<b>x</b>" })];
    mount();
    expect(await screen.findByRole("link", { name: "<img src=x onerror=alert(1)>" })).toBeTruthy();
    expect(document.querySelector("img")).toBeNull();
    expect(document.querySelector("b")).toBeNull();
  });
});
