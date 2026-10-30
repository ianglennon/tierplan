// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { useState } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { EnvironmentItem, HealthState } from "@/api/environments";
import { TooltipProvider } from "@/components/ui/tooltip";
import { EnvOverviewTable } from "@/features/environments/overview-table";
import type { SortDir, SortKey } from "@/features/environments/sort";
import "@/i18n";

function env(id: string, name: string, state: HealthState, extra: Partial<EnvironmentItem> = {}) {
  const item: EnvironmentItem = {
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
  return item;
}

const SWARM = env("s1", "prod swarm", "online", {
  envType: "swarm",
  nodes: { ready: 3, total: 4 },
  agents: { connected: 3, expected: 4 },
  failingServices: { failing: 1, total: 6 },
  agentVersion: "0.2.0",
  updateAvailable: true,
  engineVersion: "29.8.2",
  lastSeen: "2026-10-03T10:00:00Z",
});
const STANDALONE = env("h1", "nas", "online", {
  envType: "standalone",
  nodes: { ready: 1, total: 1 },
  agentVersion: "0.2.0",
  engineVersion: "28.5.1",
});
const PENDING = env("p1", "new host", "pending");
const OFFLINE = env("o1", "edge", "offline", {
  envType: "standalone",
  nodes: { ready: 0, total: 1 },
  status: { state: "offline", reason: "agent_offline", since: "2026-10-03T09:00:00Z" },
  lastSeen: "2026-10-03T09:00:00Z",
});

interface Props {
  items: EnvironmentItem[];
  loading?: boolean;
  sort?: SortKey;
  dir?: SortDir;
  onSortChange?: (sort: SortKey, dir: SortDir) => void;
  canFinishSetup?: (envId: string) => boolean;
  onFinishSetup?: (envId: string) => void;
}

async function mount(initial: Props) {
  let set: (p: Props) => void = () => {};
  function Harness() {
    const [p, setP] = useState(initial);
    set = setP;
    return (
      <TooltipProvider>
        <EnvOverviewTable onSortChange={() => {}} {...p} />
      </TooltipProvider>
    );
  }
  const root = createRootRoute({ component: Harness });
  const envRoute = createRoute({
    getParentRoute: () => root,
    path: "/env/$envId",
    component: () => null,
  });
  const router = createRouter({
    routeTree: root.addChildren([envRoute]),
    history: createMemoryHistory({ initialEntries: ["/"] }),
  });
  render(<RouterProvider router={router} />);
  await screen.findByRole("table");
  return {
    update: (p: Props) =>
      act(() => {
        set(p);
      }),
  };
}

const rowNames = () =>
  within(screen.getByRole("table"))
    .getAllByRole("link")
    .map((a) => a.textContent);

function setWidth(width: number) {
  Object.defineProperty(window, "innerWidth", { configurable: true, writable: true, value: width });
}

beforeEach(() => {
  setWidth(1024);
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  setWidth(1024);
});

describe("EnvOverviewTable columns and cells", () => {
  it("has the nine columns in order and a sort button on exactly five of them", async () => {
    await mount({ items: [SWARM] });
    const heads = screen.getAllByRole("columnheader").map((h) => h.textContent);
    expect(heads).toEqual([
      "Health",
      "Name",
      "Type",
      "Nodes",
      "Failing services",
      "Agents",
      "Engine",
      "Agent",
      "Last seen",
    ]);
    const names = screen.getAllByRole("button").map((b) => b.getAttribute("aria-label"));
    expect(names).toEqual([
      "Sort by Health",
      "Sort by Name",
      "Sort by Nodes",
      "Sort by Failing services",
      "Sort by Last seen",
    ]);
  });

  it("shows nodes and agents as ready/total with a spoken form", async () => {
    await mount({ items: [SWARM] });
    const nodes = screen.getByLabelText("3 of 4 nodes ready");
    expect(nodes.textContent).toBe("3/4");
    expect(nodes.className).toContain("tabular-nums");
    expect(screen.getByLabelText("3 of 4 agents connected").textContent).toBe("3/4");
    expect(screen.getByLabelText("1 of 6 services failing").textContent).toBe("1/6");
  });

  it("uses the singular for one node", async () => {
    await mount({ items: [STANDALONE] });
    expect(screen.getByLabelText("1 of 1 node ready").textContent).toBe("1/1");
  });

  it("shows a dash for a cell with no meaning, and a zero as 0", async () => {
    await mount({
      items: [
        STANDALONE,
        env("z1", "zero", "online", {
          nodes: { ready: 0, total: 0 },
          failingServices: { failing: 0, total: 0 },
        }),
      ],
    });
    const rows = screen.getAllByRole("row").slice(1);
    const standalone = rows.find((r) => within(r).queryByText("nas"));
    // Agents is a dash for a standalone host; failing is a dash until known.
    const cells = within(standalone as HTMLElement).getAllByRole("cell");
    expect(cells[4]?.textContent).toBe("—");
    expect(cells[5]?.textContent).toBe("—");
    const zero = rows.find((r) => within(r).queryByText("zero")) as HTMLElement;
    expect(within(zero).getByLabelText("0 of 0 nodes ready").textContent).toBe("0/0");
    expect(within(zero).getByLabelText("0 of 0 services failing").textContent).toBe("0/0");
  });

  it("shows a pending row's nodes and engine as dashes and offers Finish setup to token holders", async () => {
    const onFinishSetup = vi.fn();
    await mount({
      items: [PENDING],
      canFinishSetup: (id) => id === "p1",
      onFinishSetup,
    });
    const row = screen.getAllByRole("row")[1] as HTMLElement;
    const cells = within(row).getAllByRole("cell");
    expect(cells[3]?.textContent).toBe("—");
    expect(cells[6]?.textContent).toBe("—");
    expect(within(row).getByText("Pending")).toBeTruthy();
    fireEvent.click(within(row).getByRole("button", { name: "Finish setup" }));
    expect(onFinishSetup).toHaveBeenCalledWith(
      "p1",
      within(row).getByRole("button", { name: "Finish setup" }),
    );
  });

  it("leaves Finish setup out for someone without the token permission", async () => {
    await mount({ items: [PENDING], canFinishSetup: () => false, onFinishSetup: vi.fn() });
    expect(screen.queryByRole("button", { name: "Finish setup" })).toBeNull();
  });

  it("shows the agent version with an Update badge only when an update is available", async () => {
    await mount({ items: [SWARM, STANDALONE] });
    const badges = screen.getAllByLabelText("Agent update available");
    expect(badges).toHaveLength(1);
    expect(badges[0]?.textContent).toBe("Update");
    expect(screen.getAllByText("0.2.0")).toHaveLength(2);
  });

  it("reads Now for an online environment's last seen and a relative time otherwise", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2026-10-03T10:30:00Z"));
    await mount({ items: [SWARM, OFFLINE] });
    const rows = screen.getAllByRole("row").slice(1);
    const online = rows.find((r) => within(r).queryByText("prod swarm")) as HTMLElement;
    const offline = rows.find((r) => within(r).queryByText("edge")) as HTMLElement;
    expect(within(online).getByText("Now")).toBeTruthy();
    expect(within(offline).getByText("1 h ago")).toBeTruthy();
  });

  it("shows the type badges and a local socket host's second badge", async () => {
    await mount({
      items: [
        env("l1", "this host", "online", { envType: "swarm", isLocalSocket: true, kind: "local" }),
        STANDALONE,
      ],
    });
    expect(screen.getByText("Local socket")).toBeTruthy();
    expect(screen.getByText("Swarm")).toBeTruthy();
    expect(screen.getByText("Standalone")).toBeTruthy();
  });

  it("links the name to the environment and cuts a long name with its full text in a title", async () => {
    const long = "a-very-long-environment-name-".repeat(4);
    await mount({ items: [env("e9", long, "online")] });
    const link = screen.getByRole("link", { name: long });
    expect(link.getAttribute("href")).toBe("/env/e9");
    expect(link.getAttribute("title")).toBe(long);
    expect(link.className).toContain("truncate");
  });

  it("lists two environments with the same name", async () => {
    await mount({ items: [env("b", "prod", "online"), env("a", "prod", "online")] });
    expect(rowNames()).toEqual(["prod", "prod"]);
  });

  it("renders an agent-supplied name as text, never as markup", async () => {
    const hostile = '<img src=x onerror="alert(1)">';
    await mount({ items: [env("x", hostile, "online")] });
    expect(screen.getByRole("link", { name: hostile })).toBeTruthy();
    expect(document.querySelector("img")).toBeNull();
  });

  it("shows six 36px skeleton rows while loading", async () => {
    await mount({ items: [], loading: true });
    const body = screen.getByRole("table").querySelector("tbody") as HTMLElement;
    const rows = body.querySelectorAll("tr");
    expect(rows).toHaveLength(6);
    expect(rows[0]?.className).toContain("h-9");
    expect(screen.getByRole("table").getAttribute("aria-busy")).toBe("true");
  });

  it("keeps a sticky header on every row count", async () => {
    const many = Array.from({ length: 100 }, (_, i) => env(`e${i}`, `env ${i}`, "online"));
    await mount({ items: many });
    expect(screen.getAllByRole("row")).toHaveLength(101);
    const head = screen.getAllByRole("columnheader")[0] as HTMLElement;
    expect(head.className).toContain("sticky");
    expect(head.className).toContain("top-0");
  });
});

describe("EnvOverviewTable sorting", () => {
  it("orders problems first by default and marks Health as the ascending sort", async () => {
    await mount({ items: [STANDALONE, OFFLINE, PENDING] });
    expect(rowNames()).toEqual(["edge", "new host", "nas"]);
    const health = screen.getAllByRole("columnheader")[0] as HTMLElement;
    expect(health.getAttribute("aria-sort")).toBe("ascending");
  });

  it("asks for Name ascending first, then descending, and marks aria-sort", async () => {
    const onSortChange = vi.fn();
    const view = await mount({ items: [STANDALONE, OFFLINE], onSortChange });
    fireEvent.click(screen.getByRole("button", { name: "Sort by Name" }));
    expect(onSortChange).toHaveBeenLastCalledWith("name", "asc");
    await view.update({ items: [STANDALONE, OFFLINE], onSortChange, sort: "name", dir: "asc" });
    const name = screen.getAllByRole("columnheader")[1] as HTMLElement;
    expect(name.getAttribute("aria-sort")).toBe("ascending");
    expect(rowNames()).toEqual(["edge", "nas"]);
    fireEvent.click(screen.getByRole("button", { name: "Sort by Name" }));
    expect(onSortChange).toHaveBeenLastCalledWith("name", "desc");
    await view.update({ items: [STANDALONE, OFFLINE], onSortChange, sort: "name", dir: "desc" });
    expect(name.getAttribute("aria-sort")).toBe("descending");
    expect(rowNames()).toEqual(["nas", "edge"]);
  });

  it("marks the other sortable headers as not sorted", async () => {
    await mount({ items: [STANDALONE], sort: "name", dir: "asc" });
    const heads = screen.getAllByRole("columnheader");
    expect(heads[3]?.getAttribute("aria-sort")).toBe("none");
    expect(heads[2]?.getAttribute("aria-sort")).toBeNull();
  });
});

describe("EnvOverviewTable health cell", () => {
  it("makes a non-online health cell focusable and shows the reason and a fix hint", async () => {
    await mount({ items: [OFFLINE, STANDALONE] });
    const rows = screen.getAllByRole("row").slice(1);
    const off = rows.find((r) => within(r).queryByText("edge")) as HTMLElement;
    const on = rows.find((r) => within(r).queryByText("nas")) as HTMLElement;
    const trigger = within(off)
      .getByText(/^Offline since/)
      .closest("[tabindex]") as HTMLElement;
    expect(trigger.getAttribute("tabindex")).toBe("0");
    expect(within(on).getByText("Online").closest("[tabindex]")).toBeNull();
    await act(async () => {
      fireEvent.focus(trigger);
    });
    expect(screen.getAllByText("The agent isn't connected.").length).toBeGreaterThan(0);
    expect(screen.getAllByText(/It reconnects by itself/).length).toBeGreaterThan(0);
  });

  it("words every reason with ReasonText: its text and hint, with a path in the hint as code", async () => {
    await mount({
      items: [
        env("g1", "gone", "offline", {
          status: { state: "offline", reason: "engine_unreachable", since: "2026-10-03T09:00:00Z" },
        }),
      ],
    });
    const trigger = screen.getByText(/^Offline since/).closest("[tabindex]") as HTMLElement;
    await act(async () => {
      fireEvent.focus(trigger);
    });
    const tip = (await screen.findAllByText("The agent can't reach Docker."))[0] as HTMLElement;
    const box = tip.closest("[data-slot=tooltip-content]") as HTMLElement;
    expect(box.textContent).toContain("that the Docker daemon is running");
    expect(within(box).getByText("/var/run/docker.sock").tagName).toBe("CODE");
  });

  it("never puts a command in the tooltip, for a reason whose full view has one", async () => {
    await mount({ items: [OFFLINE] });
    const trigger = screen.getByText(/^Offline since/).closest("[tabindex]") as HTMLElement;
    await act(async () => {
      fireEvent.focus(trigger);
    });
    await screen.findAllByText("The agent isn't connected.");
    expect(screen.queryByText(/docker ps/)).toBeNull();
    expect(screen.queryByText(/docker service ps/)).toBeNull();
  });

  it("words the local socket's codes, which the agent codes do not cover", async () => {
    await mount({
      items: [
        env("l1", "this host", "offline", {
          kind: "local",
          isLocalSocket: true,
          status: { state: "offline", reason: "unreachable", since: "2026-10-03T09:00:00Z" },
        }),
      ],
    });
    const trigger = screen.getByText(/^Offline since/).closest("[tabindex]") as HTMLElement;
    await act(async () => {
      fireEvent.focus(trigger);
    });
    expect(
      (await screen.findAllByText("Docker isn't answering on the local socket."))[0],
    ).toBeTruthy();
  });

  it("falls back to wording for the state when the reason is not one it knows", async () => {
    await mount({
      items: [
        env("d1", "odd", "degraded", { status: { state: "degraded", reason: "from_the_future" } }),
      ],
    });
    const trigger = screen.getByText("Degraded").closest("[tabindex]") as HTMLElement;
    await act(async () => {
      fireEvent.focus(trigger);
    });
    expect(
      screen.getAllByText("Something is wrong on part of this environment.").length,
    ).toBeGreaterThan(0);
  });
});

describe("EnvOverviewTable under 768px", () => {
  it("renders stacked rows with the reason on line 3 and no table", async () => {
    setWidth(375);
    const root = createRootRoute({
      component: () => (
        <TooltipProvider>
          <EnvOverviewTable items={[SWARM, OFFLINE]} onSortChange={() => {}} />
        </TooltipProvider>
      ),
    });
    const router = createRouter({
      routeTree: root,
      history: createMemoryHistory({ initialEntries: ["/"] }),
    });
    render(<RouterProvider router={router} />);
    const list = await screen.findByRole("list");
    expect(screen.queryByRole("table")).toBeNull();
    const items = within(list).getAllByRole("listitem");
    expect(items).toHaveLength(2);
    // Problems first: the offline one leads, with its reason on line 3.
    const first = items[0] as HTMLElement;
    expect(within(first).getByRole("link", { name: "edge" })).toBeTruthy();
    expect(within(first).getByText("The agent isn't connected.")).toBeTruthy();
    expect(first.className).toContain("min-h-11");
    // The whole row is the tap target, not just the line of text.
    expect(first.className).toContain("relative");
    expect(within(first).getByRole("link", { name: "edge" }).className).toContain("after:absolute");
    // Line 3 is ReasonText's compact variant: the text and the hint, two lines.
    expect(
      within(first).getByText(/It reconnects by itself when it can reach Coaming\./),
    ).toBeTruthy();
    const line = within(first).getByText("The agent isn't connected.").closest("p");
    expect(line?.textContent).not.toContain("reconnects by itself");
    const second = items[1] as HTMLElement;
    expect(within(second).getByText("3/4 nodes · 1 failing · agent 0.2.0")).toBeTruthy();
    expect(within(second).queryByText(/isn't connected/)).toBeNull();
  });
});

describe("EnvOverviewTable live updates", () => {
  it("updates a row in place and waits to re-sort while the pointer is in the table", async () => {
    const a = env("a", "alpha", "online");
    const b = env("b", "bravo", "offline", {
      status: { state: "offline", reason: "agent_offline" },
    });
    const view = await mount({ items: [a, b] });
    expect(rowNames()).toEqual(["bravo", "alpha"]);

    const wrapper = screen.getByRole("table").parentElement as HTMLElement;
    fireEvent.pointerEnter(wrapper);
    await view.update({
      items: [
        env("a", "alpha", "offline", { status: { state: "offline", reason: "agent_offline" } }),
        env("b", "bravo", "online"),
      ],
    });
    // Same order, new content: bravo is online now and still first.
    expect(rowNames()).toEqual(["bravo", "alpha"]);
    const first = screen.getAllByRole("row")[1] as HTMLElement;
    expect(within(first).getByText("Online")).toBeTruthy();

    fireEvent.pointerLeave(wrapper);
    expect(rowNames()).toEqual(["alpha", "bravo"]);
  });

  it("waits to re-sort while focus is inside the table", async () => {
    const view = await mount({
      items: [
        env("a", "alpha", "online"),
        env("b", "bravo", "offline", { status: { state: "offline" } }),
      ],
    });
    const link = screen.getByRole("link", { name: "alpha" });
    act(() => link.focus());
    await view.update({
      items: [
        env("a", "alpha", "offline", { status: { state: "offline" } }),
        env("b", "bravo", "online"),
      ],
    });
    expect(rowNames()).toEqual(["bravo", "alpha"]);
    act(() => link.blur());
    expect(rowNames()).toEqual(["alpha", "bravo"]);
  });

  it("applies a new sort at once even while the pointer is in the table", async () => {
    const items = [env("a", "alpha", "online"), env("b", "bravo", "online")];
    const view = await mount({ items, sort: "name", dir: "asc" });
    fireEvent.pointerEnter(screen.getByRole("table").parentElement as HTMLElement);
    await view.update({ items, sort: "name", dir: "desc" });
    expect(rowNames()).toEqual(["bravo", "alpha"]);
  });

  it("flashes a changed row muted for 150 ms and no other row", async () => {
    const a = env("a", "alpha", "online");
    const b = env("b", "bravo", "online");
    const view = await mount({ items: [a, b] });
    // After the mount: testing-library's waits do not advance fake timers.
    vi.useFakeTimers();
    await view.update({
      items: [
        env("a", "alpha", "degraded", { status: { state: "degraded", reason: "clock_skew" } }),
        b,
      ],
    });
    const rows = screen.getAllByRole("row").slice(1);
    const alpha = rows.find((r) => within(r).queryByText("alpha")) as HTMLElement;
    const bravo = rows.find((r) => within(r).queryByText("bravo")) as HTMLElement;
    expect(alpha.getAttribute("data-flash")).toBe("true");
    expect(bravo.getAttribute("data-flash")).toBeNull();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(160);
    });
    expect(alpha.getAttribute("data-flash")).toBeNull();
  });

  it("does not flash under prefers-reduced-motion", async () => {
    vi.stubGlobal("matchMedia", (query: string) => ({
      matches: query.includes("prefers-reduced-motion"),
      media: query,
      addEventListener: () => {},
      removeEventListener: () => {},
    }));
    const view = await mount({ items: [env("a", "alpha", "online")] });
    await view.update({
      items: [env("a", "alpha", "offline", { status: { state: "offline" } })],
    });
    expect(screen.getAllByRole("row")[1]?.getAttribute("data-flash")).toBeNull();
  });

  it("announces a state change politely and at most once every 5 seconds", async () => {
    const a = env("a", "alpha", "online");
    const b = env("b", "bravo", "online");
    const view = await mount({ items: [a, b] });
    // After the mount: testing-library's waits do not advance fake timers.
    vi.useFakeTimers();
    const region = () => document.querySelector("[aria-live=polite]") as HTMLElement;
    expect(region().textContent).toBe("");

    await view.update({
      items: [env("a", "alpha", "offline", { status: { state: "offline" } }), b],
    });
    expect(region().textContent).toBe("alpha is now Offline");

    // A second change inside the window waits for it to end.
    await view.update({
      items: [
        env("a", "alpha", "offline", { status: { state: "offline" } }),
        env("b", "bravo", "degraded", { status: { state: "degraded" } }),
      ],
    });
    expect(region().textContent).toBe("alpha is now Offline");
    await act(async () => {
      await vi.advanceTimersByTimeAsync(5_100);
    });
    expect(region().textContent).toBe("bravo is now Degraded");
  });

  it("announces nothing for the first list it is given", async () => {
    await mount({ items: [OFFLINE, PENDING] });
    expect((document.querySelector("[aria-live=polite]") as HTMLElement).textContent).toBe("");
  });
});
