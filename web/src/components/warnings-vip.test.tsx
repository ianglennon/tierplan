// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  Outlet,
  RouterProvider,
} from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { queryClient } from "@/api/query-client";
import { HealthBell } from "@/components/warnings";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { EnvHealthStreamProvider } from "@/features/environments/health-stream";
import { meQueryOptions } from "@/hooks/use-me";
import "@/i18n";
import { makeMe } from "@/test-me";

// An Admin of one environment reads its bell. The server sends the virtual IP items only to such a caller
// (26-26); the web draws whatever it is sent.
const READER = makeMe({
  permissions: { global: [], environments: { "*": ["environments.read", "ingress.manage"] } },
});

let envItems: Array<Record<string, unknown>> = [];

beforeEach(() => {
  envItems = [];
  setCsrfToken("csrf-1");
  FakeEventSource.reset();
  vi.stubGlobal("EventSource", FakeEventSource);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(READER);
      if (path === "/api/warnings") return Response.json({ items: [] });
      if (path === "/api/environments/warnings") return Response.json({ items: envItems });
      if (path === "/api/registries/alerts") return Response.json({ items: [] });
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  queryClient.clear();
  setCsrfToken(null);
});

function renderBell() {
  queryClient.setQueryData(meQueryOptions.queryKey, READER);
  const root = createRootRoute({
    component: () => (
      <>
        <HealthBell />
        <Outlet />
      </>
    ),
  });
  const home = createRoute({ getParentRoute: () => root, path: "/", component: () => null });
  const router = createRouter({
    routeTree: root.addChildren([home]),
    history: createMemoryHistory({ initialEntries: ["/"] }),
  });
  render(
    <QueryClientProvider client={queryClient}>
      <EnvHealthStreamProvider>
        <RouterProvider router={router} />
      </EnvHealthStreamProvider>
    </QueryClientProvider>,
  );
}

// ago is an instant this long before now, which the bell words as "3 h ago" and the like.
const ago = (ms: number) => new Date(Date.now() - ms).toISOString();
const HOUR = 3_600_000;

interface Drawn {
  id: string;
  title: string;
  body: string;
  el: HTMLElement;
}

// The bell opened on the items the server sent, and what it drew, in order.
async function drawn(): Promise<Drawn[]> {
  renderBell();
  fireEvent.click(await screen.findByRole("button", { name: /^Warnings, \d+ items?$/ }));
  const els = await screen.findAllByRole("listitem");
  return els.map((el) => {
    const title = el.querySelector("[title]")?.textContent ?? "";
    return {
      id: el.getAttribute("data-warning") ?? "",
      title,
      body: el.textContent?.slice(title.length) ?? "",
      el,
    };
  });
}

async function itemOf(id: string): Promise<Drawn> {
  const items = await drawn();
  expect(items).toHaveLength(1);
  const [item] = items;
  if (!item) throw new Error("no item drawn");
  expect(item.id).toBe(id);
  return item;
}

// What the server sends for one environment's item.
const item = (id: string, rest: Record<string, unknown> = {}, severity = "warning") => ({
  id,
  severity,
  envId: "e1",
  envName: "Prod swarm",
  since: ago(2 * HOUR),
  ...rest,
});

describe("virtual IP bell items (tracer)", () => {
  it("draws an address nobody holds: its title, its words and nothing to click", async () => {
    envItems = [
      item(
        "vip_no_holder",
        { vip: { address: "192.168.1.250/24" }, since: ago(3 * HOUR) },
        "error",
      ),
    ];
    const { title, body, el } = await itemOf("vip_no_holder");
    expect(title).toBe("No node holds the virtual IP on Prod swarm");
    expect(body).toBe(
      "No node holds 192.168.1.250/24, so ingress doesn't answer on it. This began 3 h ago. Check that a proxy is ready on at least one node.",
    );
    expect(el.querySelector("a, button")).toBeNull();
  });
});

describe("virtual IP bell items (empty)", () => {
  it("draws nothing for an environment with no virtual IP condition", async () => {
    renderBell();
    fireEvent.click(await screen.findByRole("button", { name: "Warnings" }));
    expect(await screen.findByText("No warnings")).toBeTruthy();
  });
});

describe("virtual IP bell items (each ID)", () => {
  it("vip_multiple_holders reads one node and many nodes with their own forms", async () => {
    envItems = [item("vip_multiple_holders", { count: 3, vip: { address: "10.0.0.9/24" } })];
    const many = await itemOf("vip_multiple_holders");
    expect(many.title).toBe("More than one node holds the virtual IP on Prod swarm");
    expect(many.body).toBe(
      "3 nodes all report holding 10.0.0.9/24. The nodes probably can't hear each other. Check that multicast traffic passes between them on the network.",
    );
    cleanup();
    envItems = [item("vip_multiple_holders", { count: 1, vip: { address: "10.0.0.9/24" } })];
    expect((await itemOf("vip_multiple_holders")).body).toBe("1 node reports holding 10.0.0.9/24.");
  });

  it("vip_holder_unheard names the holder, the address and when it was last heard", async () => {
    envItems = [
      item("vip_holder_unheard", {
        node: "node-a",
        vip: { address: "192.168.1.250/24" },
        since: ago(5 * 60_000),
      }),
    ];
    const { title, body, el } = await itemOf("vip_holder_unheard");
    expect(title).toBe("TierPlan can't hear the node that holds the virtual IP on Prod swarm");
    expect(body).toBe(
      "node-a was the last to hold 192.168.1.250/24. TierPlan last heard from it 5 min ago, and no other node has taken the address since. If node-a is down, ingress doesn't answer on 192.168.1.250/24.",
    );
    expect(el.querySelector("a, button")).toBeNull();
  });

  it("vip_moved names the latest move once, and counts several", async () => {
    envItems = [
      item("vip_moved", {
        count: 1,
        vip: { from: "node-a", to: "node-b", moveReason: "holder_failed" },
        since: ago(5 * 60_000),
      }),
    ];
    const one = await itemOf("vip_moved");
    expect(one.title).toBe("The virtual IP moved on Prod swarm");
    expect(one.body).toBe(
      "It moved from node-a to node-b 5 min ago. The previous holder stopped answering.",
    );
    cleanup();
    envItems = [
      item("vip_moved", {
        count: 3,
        vip: { from: "node-a", to: "node-b", moveReason: "probe_failing" },
        since: ago(5 * 60_000),
      }),
    ];
    const many = await itemOf("vip_moved");
    expect(many.title).toBe("The virtual IP moved 3 times on Prod swarm");
    expect(many.body).toBe(
      "The latest move was from node-a to node-b 5 min ago. The previous holder's proxy stopped answering.",
    );
  });

  it("vip_moved gives each reason its own sentence and reads an empty side as no node", async () => {
    const sentences: Record<string, string> = {
      holder_failed: "The previous holder stopped answering.",
      probe_failing: "The previous holder's proxy stopped answering.",
      drain_handoff: "The previous holder was drained.",
      unknown: "TierPlan couldn't tell why.",
      // A reason this build has no words for is the unknown one.
      from_the_future: "TierPlan couldn't tell why.",
    };
    for (const [moveReason, sentence] of Object.entries(sentences)) {
      envItems = [item("vip_moved", { count: 1, vip: { to: "node-b", moveReason } })];
      const { body } = await itemOf("vip_moved");
      expect(body).toBe(`It moved from no node to node-b 2 h ago. ${sentence}`);
      cleanup();
    }
    envItems = [item("vip_moved", { count: 1, vip: { from: "node-a", moveReason: "unknown" } })];
    expect((await itemOf("vip_moved")).body).toBe(
      "It moved from node-a to no node 2 h ago. TierPlan couldn't tell why.",
    );
  });

  it("vip_foreign_router names the sender when the server could validate one", async () => {
    envItems = [item("vip_foreign_router", { vip: { routerId: 51, sender: "192.168.1.7" } })];
    const withSender = await itemOf("vip_foreign_router");
    expect(withSender.title).toBe("Another router uses the virtual IP's router ID on Prod swarm");
    expect(withSender.body).toBe(
      "192.168.1.7 sends virtual router adverts with router ID 51 that this swarm's key doesn't match. Change this swarm's router ID, or find that device.",
    );
    cleanup();
    envItems = [item("vip_foreign_router", { vip: { routerId: 51 } })];
    expect((await itemOf("vip_foreign_router")).body).toBe(
      "Something on the network sends virtual router adverts with router ID 51 that this swarm's key doesn't match. Change this swarm's router ID, or find the other router.",
    );
  });

  it("vip_clock_skew gives the offset and the window, or says the clock is rejected without one", async () => {
    envItems = [
      item("vip_clock_skew", { node: "node-c", offsetSeconds: -17.6, vip: { windowSeconds: 30 } }),
    ];
    const withOffset = await itemOf("vip_clock_skew");
    expect(withOffset.title).toBe("The clock is off on node-c, which the virtual IP needs");
    expect(withOffset.body).toBe(
      "node-c on Prod swarm is 17 s out of step. The virtual IP rejects adverts more than 30 s old, so the nodes may stop hearing each other. Fix the clock on that node.",
    );
    cleanup();
    envItems = [item("vip_clock_skew", { node: "node-c", vip: { windowSeconds: 30 } })];
    expect((await itemOf("vip_clock_skew")).body).toBe(
      "node-c on Prod swarm is rejecting adverts as too old, so its clock is out of step. The virtual IP can't stay up while that lasts. Fix the clock on that node.",
    );
  });

  it("vip_privileged_lost names one manager, and counts several", async () => {
    envItems = [item("vip_privileged_lost", { count: 1, node: "manager-1" })];
    const one = await itemOf("vip_privileged_lost");
    expect(one.title).toBe("The virtual IP can't be managed on Prod swarm");
    expect(one.body).toBe(
      "The agent on manager-1 no longer allows privileged services. The virtual IP keeps running, but TierPlan can't change or repair it until every manager's agent allows them again.",
    );
    cleanup();
    envItems = [item("vip_privileged_lost", { count: 2 })];
    expect((await itemOf("vip_privileged_lost")).body).toBe(
      "2 managers' agents no longer allow privileged services. The virtual IP keeps running, but TierPlan can't change or repair it until every manager's agent allows them again.",
    );
  });
});

describe("virtual IP bell items (populated, partial, overflow)", () => {
  it("draws one item per ID, and one clock item per node", async () => {
    envItems = [
      item("vip_no_holder", { vip: { address: "10.0.0.9/24" } }, "error"),
      item("vip_moved", { count: 1, vip: { from: "a", to: "b", moveReason: "unknown" } }),
      item("vip_clock_skew", { node: "node-a", offsetSeconds: 20, vip: { windowSeconds: 30 } }),
      item("vip_clock_skew", { node: "node-b", offsetSeconds: 25, vip: { windowSeconds: 30 } }),
    ];
    const items = await drawn();
    expect(items.map((i) => i.id)).toEqual([
      "vip_no_holder",
      "vip_moved",
      "vip_clock_skew",
      "vip_clock_skew",
    ]);
    expect(items.slice(2).map((i) => i.title)).toEqual([
      "The clock is off on node-a, which the virtual IP needs",
      "The clock is off on node-b, which the virtual IP needs",
    ]);
  });

  it("never draws 'undefined', a blank or a double space when facts are missing", async () => {
    envItems = [
      item("vip_no_holder", { vip: { address: "10.0.0.9/24" }, since: undefined }, "error"),
      item("vip_multiple_holders", { count: 2, vip: { address: "10.0.0.9/24" } }),
      item("vip_moved", { count: 1, vip: {} }),
      item("vip_foreign_router", { vip: { routerId: 7 } }),
      item("vip_clock_skew", { node: "node-a" }),
      item("vip_privileged_lost", { count: 3 }),
    ];
    const items = await drawn();
    expect(items).toHaveLength(6);
    for (const { id, title, body } of items) {
      for (const text of [title, body]) {
        expect(text, id).not.toBe("");
        expect(text, id).not.toMatch(/undefined|null|NaN|\{\{/);
        expect(text, id).not.toMatch(/ {2}| [.,]/);
        expect(text, id).toBe(text.trim());
      }
    }
  });

  it("wraps a 64-character name in the body and truncates the title with its whole text on hover", async () => {
    const long = "e".repeat(64);
    envItems = [
      item("vip_multiple_holders", { envName: long, count: 2, vip: { address: "10.0.0.9/24" } }),
    ];
    const { el, title } = await itemOf("vip_multiple_holders");
    expect(title).toBe(`More than one node holds the virtual IP on ${long}`);
    const titleEl = el.querySelector("[title]");
    expect(titleEl?.getAttribute("title")).toBe(title);
    expect(titleEl?.className).toContain("truncate");
    const bodyEl = el.querySelector("[data-slot=item-description]");
    expect(bodyEl?.className).toContain("wrap-anywhere");
    expect(bodyEl?.className).toContain("line-clamp-none");
  });
});
