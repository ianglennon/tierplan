// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import {
  PLACEMENT_REFETCH_MS,
  placementKey,
  placementQuery,
  placementRetry,
} from "@/api/placement";
import { queryClient } from "@/api/query-client";
import { servicesKey } from "@/api/services";
import { TooltipProvider } from "@/components/ui/tooltip";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { node, placement, service, task } from "@/features/placement/fixtures";
import type { PlacementSearch, PlacementSearchPatch } from "@/features/placement/placement-search";
import { PlacementView } from "@/features/placement/placement-view";
import "@/i18n";
import { makeMe } from "@/test-me";
import { splitPermissions } from "@/test-permissions";
import { TestRouter } from "@/test-router";

// The Placement page: its states (E9), the filters in the URL and the polling (plan 06-11,
// RES-12, D-18, D-19, UI-SPEC "Placement views").

const ENV = "e1";
const defaults = queryClient.getDefaultOptions();

let me = makeMe({ envs: { [ENV]: "viewer" } });
let answer: () => Response | Promise<Response>;
let calls = 0;

beforeEach(() => {
  me = makeMe({ envs: { [ENV]: "viewer" } });
  calls = 0;
  answer = () => Response.json(DATA);
  queryClient.setDefaultOptions({ queries: { retry: false, refetchOnWindowFocus: false } });
  placementRetry.attempts = 0;
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      if (url.pathname === "/api/me") return Response.json(me);
      if (url.pathname === "/api/environments") {
        return Response.json({ items: [{ id: ENV, name: "Prod swarm", envType: "swarm" }] });
      }
      if (url.pathname === `/api/env/${ENV}/placement`) {
        calls++;
        return answer();
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.useRealTimers();
  queryClient.clear();
  queryClient.setDefaultOptions(defaults);
  placementRetry.attempts = 3;
});

const DATA = placement(
  [node({ id: "n1", hostname: "mgr-1" }), node({ id: "n2", hostname: "wrk-1", role: "worker" })],
  [
    task({ id: "a", nodeId: "n1", service: "web", serviceId: "svc-web", stack: "shop" }),
    task({ id: "b", nodeId: "n2", service: "db", serviceId: "svc-db", stack: "shop", slot: 1 }),
    task({ id: "c", nodeId: "n2", service: "solo", serviceId: "svc-solo", stack: "" }),
  ],
  {
    services: [
      service({ id: "svc-web", name: "web", stack: "shop" }),
      service({ id: "svc-db", name: "db", stack: "shop" }),
      service({ id: "svc-solo", name: "solo", stack: "" }),
    ],
  },
);

function renderView(
  search: PlacementSearch = {},
  onSearchChange: (p: PlacementSearchPatch) => void = () => {},
  serviceId?: string,
) {
  return render(
    <QueryClientProvider client={queryClient}>
      <TooltipProvider>
        <TestRouter>
          <PlacementView
            envId={ENV}
            search={search}
            onSearchChange={onSearchChange}
            serviceId={serviceId}
          />
        </TestRouter>
      </TooltipProvider>
    </QueryClientProvider>,
  );
}

describe("PlacementView", () => {
  it("names the page and the environment, and draws a column per node", async () => {
    renderView();
    expect(await screen.findByRole("region", { name: "mgr-1" })).toBeTruthy();
    expect(screen.getByRole("heading", { level: 1, name: "Placement" })).toBeTruthy();
    expect(screen.getByRole("region", { name: "wrk-1" })).toBeTruthy();
    expect(screen.getByText("Where tasks run across Prod swarm.")).toBeTruthy();
    expect(
      within(screen.getByRole("region", { name: "wrk-1" })).getAllByRole("link", {
        name: /^Open service/,
      }),
    ).toHaveLength(2);
  });

  it("draws the toolbar and three skeleton columns at once, before the answer", async () => {
    answer = () => new Promise<Response>(() => {});
    const { container } = renderView();
    expect(await screen.findByRole("searchbox", { name: "Search services" })).toBeTruthy();
    expect(screen.getByRole("radio", { name: "By node" })).toBeTruthy();
    expect(container.querySelectorAll("[data-slot=node-columns-skeleton] > div")).toHaveLength(3);
  });

  it("says why there is nothing when the swarm has no tasks", async () => {
    answer = () => Response.json(placement([node({ id: "n1", hostname: "mgr-1" })], []));
    renderView();
    expect(await screen.findByText("No tasks to place")).toBeTruthy();
    expect(screen.queryByRole("region")).toBeNull();
  });

  it("offers Clear filters when the filters leave nothing, and clears both", async () => {
    const change = vi.fn();
    renderView({ stack: "blog", q: "zzz" }, change);
    expect(await screen.findByText("No tasks match these filters.")).toBeTruthy();
    fireEvent.click(screen.getAllByRole("button", { name: /Clear filters/ })[0] as HTMLElement);
    expect(change).toHaveBeenCalledWith({ stack: undefined, q: undefined });
  });

  it("shows a load error with Retry loading placement, which asks again", async () => {
    answer = () => Response.json({ title: "boom", status: 500 }, { status: 500 });
    renderView();
    expect(await screen.findByText("Couldn't load placement.")).toBeTruthy();
    const before = calls;
    answer = () => Response.json(DATA);
    fireEvent.click(screen.getByRole("button", { name: "Retry loading placement" }));
    expect(await screen.findByRole("region", { name: "mgr-1" })).toBeTruthy();
    expect(calls).toBeGreaterThan(before);
  });

  it("tells a standalone host it has no swarm", async () => {
    answer = () =>
      Response.json(
        { type: "not_a_swarm", title: "Not a swarm", status: 409 },
        { status: 409, headers: { "Content-Type": "application/problem+json" } },
      );
    renderView();
    expect(await screen.findByRole("heading", { name: /swarm/i })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Retry loading placement" })).toBeNull();
  });

  it("shows no page to a user who may not read services", async () => {
    me = makeMe({
      envs: { [ENV]: "viewer" },
      permissions: splitPermissions(["containers.read", "environments.read"]),
    });
    renderView();
    await waitFor(() => expect(screen.queryByRole("heading", { name: "Placement" })).toBeNull());
    expect(calls).toBe(0);
  });

  it("filters by service name and lists the stacks in the Stack select", async () => {
    const change = vi.fn();
    renderView({ stack: "shop", q: "web" }, change);
    const region = await screen.findByRole("region", { name: "mgr-1" });
    expect(within(region).getAllByRole("link", { name: /^Open service/ })).toHaveLength(1);
    // The other node stays, empty.
    expect(
      within(screen.getByRole("region", { name: "wrk-1" })).getByText("No tasks"),
    ).toBeTruthy();
    fireEvent.click(screen.getByRole("combobox", { name: "Stack" }));
    expect(await screen.findByRole("option", { name: "All stacks" })).toBeTruthy();
    expect(screen.getByRole("option", { name: "shop" })).toBeTruthy();
    expect(screen.getByRole("option", { name: "Not in a stack" })).toBeTruthy();
    fireEvent.click(screen.getByRole("option", { name: "Not in a stack" }));
    expect(change).toHaveBeenCalledWith({ stack: "none" });
  });

  it("shows Clear filters only while a filter is on", async () => {
    const { unmount } = renderView();
    await screen.findByRole("region", { name: "mgr-1" });
    expect(screen.queryByRole("button", { name: "Clear filters" })).toBeNull();
    unmount();
    renderView({ q: "web" });
    await screen.findByRole("region", { name: "mgr-1" });
    expect(screen.getByRole("button", { name: "Clear filters" })).toBeTruthy();
  });
});

describe("the Grid layout", () => {
  it("offers By node and Grid, and a change of layout keeps the filters", async () => {
    const change = vi.fn();
    renderView({ view: "grid", stack: "shop" }, change);
    await screen.findByRole("table");
    expect(screen.getByRole("radio", { name: "Grid" }).getAttribute("aria-checked")).toBe("true");
    expect(screen.getByRole("radio", { name: "By node" }).getAttribute("aria-checked")).toBe(
      "false",
    );
    fireEvent.click(screen.getByRole("radio", { name: "By node" }));
    expect(change).toHaveBeenCalledWith({ view: undefined });
    expect(change).not.toHaveBeenCalledWith(expect.objectContaining({ stack: undefined }));
    cleanup();
    const next = vi.fn();
    renderView({}, next);
    await screen.findByRole("region", { name: "mgr-1" });
    fireEvent.click(screen.getByRole("radio", { name: "Grid" }));
    expect(next).toHaveBeenCalledWith({ view: "grid" });
  });

  it("draws the node x service table and applies the stack and search filters to its rows", async () => {
    renderView({ view: "grid", stack: "shop", q: "web" });
    const table = await screen.findByRole("table");
    expect(within(table).getByRole("rowheader", { name: "web" })).toBeTruthy();
    expect(within(table).queryByRole("rowheader", { name: "db" })).toBeNull();
    expect(
      within(table)
        .getAllByRole("columnheader")
        .map((h) => h.textContent),
    ).toEqual(["Service", "mgr-11 task", "wrk-10 tasks", "Total"]);
  });

  it("says nothing matches, and offers Clear filters, when the filters leave no row", async () => {
    renderView({ view: "grid", q: "zzz" });
    expect(await screen.findByText("No tasks match these filters.")).toBeTruthy();
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("holds the grid still while a cell popover is open, and catches up when it closes", async () => {
    renderView({ view: "grid" });
    fireEvent.click(await screen.findByRole("button", { name: "web on mgr-1: 1 task" }));
    expect(await screen.findByRole("dialog")).toBeTruthy();

    act(() => {
      queryClient.setQueryData(placementKey(ENV), {
        ...DATA,
        tasks: [
          ...(DATA.tasks ?? []),
          task({ id: "a2", nodeId: "n1", service: "web", serviceId: "svc-web", slot: 2 }),
        ],
      });
    });
    expect(screen.getByRole("button", { name: "web on mgr-1: 1 task" })).toBeTruthy();

    fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape" });
    expect(await screen.findByRole("button", { name: "web on mgr-1: 2 tasks" })).toBeTruthy();
  });
});

describe("a service's Placement tab", () => {
  const WORKERS = [
    node({ id: "n1", hostname: "mgr-1" }),
    node({ id: "n2", hostname: "mgr-2" }),
    node({ id: "n3", hostname: "mgr-3" }),
    node({ id: "n4", hostname: "wrk-1", role: "worker" }),
  ];
  const ONE = placement(WORKERS, [task({ id: "a", nodeId: "n4", slot: 1 })], {
    services: [
      service({
        constraints: ["node.role==worker"],
        preferences: ["node.labels.zone"],
        maxReplicas: 2,
      }),
    ],
  });

  it("is headed Where {name} runs, with the rules card above columns for every node", async () => {
    answer = () => Response.json(ONE);
    renderView({}, () => {}, "svc-web");
    expect(await screen.findByRole("heading", { name: "Where web runs" })).toBeTruthy();
    expect(screen.queryByRole("heading", { level: 1 })).toBeNull();
    const rules = screen.getByRole("region", { name: "Placement rules" });
    expect(within(rules).getByText("Matches 1 of 4 nodes")).toBeTruthy();
    expect(within(rules).getByText("Spread over node.labels.zone")).toBeTruthy();
    expect(within(rules).getByText("Max replicas per node: 2")).toBeTruthy();
    expect(screen.getAllByRole("region", { name: /^(mgr|wrk)-/ })).toHaveLength(4);
    // The empty columns say whose tasks they lack.
    expect(screen.getAllByText("No tasks of web")).toHaveLength(3);
    expect(
      rules.compareDocumentPosition(screen.getByRole("region", { name: "wrk-1" })) &
        Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
  });

  it("asks the swarm for that one service, and has no stack or search filter of its own", async () => {
    const seen: string[] = [];
    answer = () => Response.json(ONE);
    const fetchMock = globalThis.fetch as ReturnType<typeof vi.fn>;
    const inner = fetchMock.getMockImplementation() as (req: Request) => Promise<Response>;
    fetchMock.mockImplementation(async (req: Request) => {
      if (new URL(req.url).pathname === `/api/env/${ENV}/placement`)
        seen.push(new URL(req.url).search);
      return inner(req);
    });
    renderView({}, () => {}, "svc-web");
    await screen.findByRole("region", { name: "wrk-1" });
    expect(seen[0]).toBe("?serviceId=svc-web");
    expect(screen.queryByRole("combobox", { name: "Stack" })).toBeNull();
    expect(screen.queryByRole("searchbox")).toBeNull();
    expect(screen.getByRole("radio", { name: "Grid" })).toBeTruthy();
  });

  it("still shows every node when the service has no task, never No tasks to place", async () => {
    answer = () => Response.json(placement(WORKERS, [], { services: [service()] }));
    renderView({}, () => {}, "svc-web");
    expect(await screen.findByRole("region", { name: "wrk-1" })).toBeTruthy();
    expect(screen.queryByText("No tasks to place")).toBeNull();
    expect(screen.getAllByText("No tasks of web")).toHaveLength(4);
  });

  it("draws the grid with the service's own row", async () => {
    answer = () => Response.json(ONE);
    renderView({ view: "grid" }, () => {}, "svc-web");
    const table = await screen.findByRole("table");
    expect(within(table).getByRole("button", { name: "web on wrk-1: 1 task" })).toBeTruthy();
  });
});

describe("placementQuery", () => {
  it("is under the services list key, so a service event refreshes it", () => {
    const key = placementKey(ENV);
    expect(key.slice(0, servicesKey(ENV).length)).toEqual([...servicesKey(ENV)]);
    expect(placementKey(ENV, "svc-web")).not.toEqual(key);
  });

  it("polls every 5 s while the page is visible and not while it is hidden", () => {
    const opts = placementQuery(ENV);
    const every = opts.refetchInterval as () => number | false;
    const original = Object.getOwnPropertyDescriptor(Document.prototype, "visibilityState");
    try {
      Object.defineProperty(Document.prototype, "visibilityState", {
        configurable: true,
        get: () => "visible",
      });
      expect(every()).toBe(PLACEMENT_REFETCH_MS);
      expect(PLACEMENT_REFETCH_MS).toBe(5_000);
      Object.defineProperty(Document.prototype, "visibilityState", {
        configurable: true,
        get: () => "hidden",
      });
      expect(every()).toBe(false);
    } finally {
      if (original) Object.defineProperty(Document.prototype, "visibilityState", original);
    }
  });

  it("asks for one service when given one", async () => {
    const seen: string[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (req: Request) => {
        seen.push(new URL(req.url).search);
        return Response.json(DATA);
      }),
    );
    await act(async () => {
      await queryClient.fetchQuery(placementQuery(ENV, "svc-web"));
    });
    expect(seen).toEqual(["?serviceId=svc-web"]);
  });
});

describe("Live stats on the node columns (06-33)", () => {
  beforeEach(() => {
    FakeEventSource.reset();
    vi.stubGlobal("EventSource", FakeEventSource);
  });

  const GIB = 1024 ** 3;
  const liveSource = (): FakeEventSource => {
    const s = FakeEventSource.instances[FakeEventSource.instances.length - 1];
    if (!s) throw new Error("no node stats stream was opened");
    return s;
  };
  const round = (nodes: object[], partial = false) => ({ t: 1, partial, nodes });
  const nodeEntry = (id: string, extra: Record<string, unknown> = {}) => ({
    id,
    cpu: 160,
    mem: 2 * GIB,
    cpus: 4,
    memoryBytes: 8 * GIB,
    counted: 1,
    silent: 0,
    notReporting: false,
    ...extra,
  });

  it("has the switch, off by default, with no stream and no bars", async () => {
    renderView();
    await screen.findByRole("region", { name: "mgr-1" });
    expect(screen.getByRole("switch", { name: "Live stats" }).getAttribute("aria-checked")).toBe(
      "false",
    );
    expect(FakeEventSource.instances).toHaveLength(0);
    expect(screen.queryByRole("progressbar")).toBeNull();
  });

  it("asks for stats through the search when it is turned on and off", async () => {
    const change = vi.fn();
    renderView({}, change);
    await screen.findByRole("region", { name: "mgr-1" });
    fireEvent.click(screen.getByRole("switch", { name: "Live stats" }));
    expect(change).toHaveBeenCalledWith({ stats: 1 });
    cleanup();
    renderView({ stats: 1 }, change);
    await screen.findByRole("region", { name: "mgr-1" });
    fireEvent.click(screen.getByRole("switch", { name: "Live stats" }));
    expect(change).toHaveBeenLastCalledWith({ stats: undefined });
  });

  it("opens the node stream with the switch on and says it is waiting", async () => {
    renderView({ stats: 1 });
    await screen.findByRole("region", { name: "mgr-1" });
    expect(liveSource().url).toBe(`/api/env/${ENV}/nodes/stats/stream`);
    const mgr = screen.getByRole("region", { name: "mgr-1" });
    expect(within(mgr).getByText("Waiting for stats…")).toBeTruthy();
    expect(within(mgr).queryAllByRole("progressbar")).toHaveLength(0);
  });

  it("shows what the tasks use of each node, in words and in two bars", async () => {
    renderView({ stats: 1 });
    await screen.findByRole("region", { name: "mgr-1" });
    act(() => liveSource().sendFrame("sample", round([nodeEntry("n1")])));
    const mgr = screen.getByRole("region", { name: "mgr-1" });
    expect(within(mgr).getByText("Tasks use 40% CPU and 2 GiB of 8 GiB memory")).toBeTruthy();
    const cpu = within(mgr).getByRole("progressbar", { name: "CPU used by tasks on mgr-1" });
    const mem = within(mgr).getByRole("progressbar", { name: "Memory used by tasks on mgr-1" });
    expect(cpu.getAttribute("aria-valuenow")).toBe("40");
    expect(mem.getAttribute("aria-valuenow")).toBe("25");
  });

  it("says Not reporting in place of the bars for a node that is silent", async () => {
    renderView({ stats: 1 });
    await screen.findByRole("region", { name: "wrk-1" });
    act(() =>
      liveSource().sendFrame(
        "sample",
        round([
          nodeEntry("n1"),
          nodeEntry("n2", { notReporting: true, cpu: null, mem: 0, counted: 0, silent: 2 }),
        ]),
      ),
    );
    const wrk = screen.getByRole("region", { name: "wrk-1" });
    expect(within(wrk).getByText("Not reporting")).toBeTruthy();
    expect(within(wrk).queryAllByRole("progressbar")).toHaveLength(0);
  });

  it("never lets a bar run past full", async () => {
    renderView({ stats: 1 });
    await screen.findByRole("region", { name: "mgr-1" });
    act(() =>
      liveSource().sendFrame("sample", round([nodeEntry("n1", { cpu: 900, mem: 20 * GIB })])),
    );
    const mgr = screen.getByRole("region", { name: "mgr-1" });
    expect(
      within(mgr)
        .getByRole("progressbar", { name: "CPU used by tasks on mgr-1" })
        .getAttribute("aria-valuenow"),
    ).toBe("100");
  });

  it("closes the stream when the switch is off or the page is left", async () => {
    const { unmount } = renderView({ stats: 1 });
    await screen.findByRole("region", { name: "mgr-1" });
    const s = liveSource();
    unmount();
    expect(s.closed).toBe(true);
  });

  it("shows the switch and the bars on a service's Placement tab too", async () => {
    answer = () =>
      Response.json(
        placement([node({ id: "n1", hostname: "mgr-1" })], [], {
          services: [service({ id: "svc-web", name: "web" })],
        }),
      );
    renderView({ stats: 1 }, () => {}, "svc-web");
    await screen.findByRole("region", { name: "mgr-1" });
    expect(screen.getByRole("switch", { name: "Live stats" })).toBeTruthy();
    act(() => liveSource().sendFrame("sample", round([nodeEntry("n1")])));
    expect(screen.getByRole("progressbar", { name: "CPU used by tasks on mgr-1" })).toBeTruthy();
  });
});
