// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { FleetFailing, FleetFailingItem } from "@/api/fleet";
import { TooltipProvider } from "@/components/ui/tooltip";
import { FleetFailingTable } from "@/features/fleet/fleet-failing-table";
import { TestRouter } from "@/test-router";
import "@/i18n";

let client: QueryClient;
let answer: () => Response | Promise<Response>;
let asked: string[];

function item(extra: Partial<FleetFailingItem> = {}): FleetFailingItem {
  return {
    kind: "service",
    id: "svc1",
    name: "web",
    envId: "e1",
    envName: "prod",
    stack: "shop",
    state: "1 of 2 running",
    running: 1,
    desired: 2,
    since: "2026-11-04T09:00:00Z",
    lastError: "task failed: no such image",
    node: "",
    nodeId: "",
    ...extra,
  };
}

function body(items: FleetFailingItem[], notIncluded: FleetFailing["notIncluded"] = []) {
  return { items, notIncluded, readAt: "2026-11-04T10:00:00Z" };
}

beforeEach(() => {
  setCsrfToken(null);
  asked = [];
  answer = () => Response.json(body([item()]));
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      asked.push(`${url.pathname}${url.search}`);
      if (url.pathname === "/api/fleet/failing") return answer();
      if (url.pathname === "/api/environments") return Response.json({ items: [] });
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

function mount(props: Partial<React.ComponentProps<typeof FleetFailingTable>> = {}) {
  render(
    <QueryClientProvider client={client}>
      <TooltipProvider>
        <TestRouter>
          <FleetFailingTable env={[]} onEnvChange={() => {}} {...props} />
        </TestRouter>
      </TooltipProvider>
    </QueryClientProvider>,
  );
}

// The table once the rows have come: it is on the page, as skeleton rows, from the first render.
const table = async () => {
  const t = await screen.findByRole("table");
  await waitFor(() => expect(t.getAttribute("aria-busy")).toBeNull());
  return t;
};
const bodyRows = () => Array.from(document.querySelectorAll("tbody tr"));
const rowNames = () =>
  bodyRows().map((r) => within(r as HTMLElement).getAllByRole("link")[0]?.textContent);

describe("FleetFailingTable", () => {
  it("lists a failing service of another environment with its state, stack and error", async () => {
    mount();
    const t = await table();
    expect(within(t).getByText("1 of 2 running")).toBeTruthy();
    const name = within(t).getByRole("link", { name: "web" });
    expect(name.getAttribute("href")).toBe("/env/e1/services/svc1");
    const env = within(t).getByRole("link", { name: "prod" });
    expect(env.getAttribute("href")).toBe("/env/e1");
    expect(within(t).getByText("shop")).toBeTruthy();
    expect(within(t).getByText("task failed: no such image")).toBeTruthy();
    expect(t.querySelector("caption")?.textContent).toBe("Failing services and containers");
    for (const column of ["Name", "Environment", "Stack", "State", "Since", "Last error"]) {
      expect(within(t).getByRole("columnheader", { name: new RegExp(column) })).toBeTruthy();
    }
  });

  it("names a container's state in words and links it with its node", async () => {
    answer = () =>
      Response.json(
        body([
          item({
            kind: "container",
            id: "c1",
            name: "cache",
            state: "restarting",
            running: null,
            desired: null,
            node: "n1",
            nodeId: "node-1",
            stack: "",
            lastError: "",
          }),
        ]),
      );
    mount();
    const t = await table();
    expect(within(t).getByText("Restarting")).toBeTruthy();
    const link = within(t).getByRole("link", { name: "cache" });
    expect(link.getAttribute("href")).toBe("/env/e1/containers/c1?node=node-1");
    // No stack and no error read as a dash, never as blank.
    expect(within(t).getAllByText("—").length).toBeGreaterThanOrEqual(2);
  });

  it("orders by Since, newest first, and puts a row with no time last", async () => {
    answer = () =>
      Response.json(
        body([
          item({ id: "a", name: "old", since: "2026-11-04T08:00:00Z" }),
          item({ id: "b", name: "unknown", since: "" }),
          item({ id: "c", name: "new", since: "2026-11-04T09:30:00Z" }),
        ]),
      );
    mount();
    await table();
    expect(rowNames()).toEqual(["new", "old", "unknown"]);
    const since = screen.getByRole("button", { name: "Sort by Since" });
    fireEvent.click(since);
    expect(rowNames()).toEqual(["old", "new", "unknown"]);
  });

  it("sorts by Name and by Environment", async () => {
    answer = () =>
      Response.json(
        body([
          item({ id: "a", name: "beta", envName: "zed" }),
          item({ id: "b", name: "alpha", envName: "yak" }),
          item({ id: "c", name: "gamma", envName: "abe" }),
        ]),
      );
    mount();
    await table();
    fireEvent.click(screen.getByRole("button", { name: "Sort by Name" }));
    expect(rowNames()).toEqual(["alpha", "beta", "gamma"]);
    fireEvent.click(screen.getByRole("button", { name: "Sort by Name" }));
    expect(rowNames()).toEqual(["gamma", "beta", "alpha"]);
    fireEvent.click(screen.getByRole("button", { name: "Sort by Environment" }));
    expect(rowNames()).toEqual(["gamma", "alpha", "beta"]);
    expect(
      screen.getByRole("columnheader", { name: /Environment/ }).getAttribute("aria-sort"),
    ).toBe("ascending");
  });

  it("draws a name, a stack and an error as text, never as markup", async () => {
    answer = () =>
      Response.json(
        body([
          item({
            name: "<img src=x onerror=alert(1)>",
            stack: "<b>stack</b>",
            lastError: "<script>alert(2)</script>",
            envName: "<i>env</i>",
          }),
        ]),
      );
    mount();
    const t = await table();
    expect(within(t).getByText("<img src=x onerror=alert(1)>")).toBeTruthy();
    expect(within(t).getByText("<script>alert(2)</script>")).toBeTruthy();
    expect(t.querySelector("img, script, b, i")).toBeNull();
  });

  it("truncates a long last error and keeps all of it for the tooltip", async () => {
    const long = "x".repeat(200);
    answer = () => Response.json(body([item({ lastError: long })]));
    mount();
    const t = await table();
    const cell = within(t).getByText(long);
    expect(cell.className).toContain("truncate");
  });

  it("says Nothing is failing only when no environment is missing", async () => {
    answer = () => Response.json(body([]));
    mount();
    expect(await screen.findByText("Nothing is failing")).toBeTruthy();
    expect(
      screen.getByText(
        "Every service and container that reports to TierPlan is running as it should.",
      ),
    ).toBeTruthy();
    expect(screen.queryByRole("status")).toBeNull();
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("does not claim more than it saw when an environment did not report", async () => {
    answer = () =>
      Response.json(body([], [{ envId: "e2", envName: "edge", reason: "offline", nodes: 0 }]));
    mount();
    expect(
      await screen.findByText("Nothing is failing in the environments that reported"),
    ).toBeTruthy();
    expect(screen.queryByText("Nothing is failing")).toBeNull();
    expect(screen.getByRole("status").textContent).toContain("edge (offline)");
  });

  it("shows the coverage alert above a table that has rows", async () => {
    answer = () =>
      Response.json(
        body([item()], [{ envId: "e2", envName: "edge", reason: "nodes_not_reporting", nodes: 2 }]),
      );
    mount();
    await table();
    expect(screen.getByRole("status").textContent).toContain("edge (2 nodes not reporting)");
  });

  it("shows six skeleton rows at 36px while loading, and the header at once", async () => {
    answer = () => new Promise<Response>(() => {});
    mount();
    const t = await screen.findByRole("table");
    expect(t.getAttribute("aria-busy")).toBe("true");
    expect(bodyRows()).toHaveLength(6);
    expect(bodyRows()[0]?.className).toContain("h-9");
    expect(within(t).getByRole("columnheader", { name: /Name/ })).toBeTruthy();
    expect(screen.queryByText("Nothing is failing")).toBeNull();
  });

  it("shows Couldn't load this view with Retry loading, and Retry reads again", async () => {
    answer = () => new Response("boom", { status: 500 });
    mount();
    expect(await screen.findByText("Couldn't load this view.")).toBeTruthy();
    expect(screen.getByText("Check the connection and try again.")).toBeTruthy();
    expect(screen.queryByText("Nothing is failing")).toBeNull();
    answer = () => Response.json(body([item()]));
    fireEvent.click(screen.getByRole("button", { name: "Retry loading" }));
    expect(await table()).toBeTruthy();
    expect(screen.queryByText("Couldn't load this view.")).toBeNull();
  });

  it("asks for the environments it is narrowed to", async () => {
    mount({
      env: ["0190a1b2-0000-7000-8000-000000000001", "0190a1b2-0000-7000-8000-000000000002"],
    });
    await table();
    const read = asked.find((a) => a.startsWith("/api/fleet/failing")) ?? "";
    const query = new URLSearchParams(read.split("?")[1]);
    expect(query.getAll("env")).toEqual([
      "0190a1b2-0000-7000-8000-000000000001",
      "0190a1b2-0000-7000-8000-000000000002",
    ]);
  });
});

describe("a long list", () => {
  const saved: Record<string, PropertyDescriptor | undefined> = {};
  const lay = (prop: string, value: number) => {
    saved[prop] = Object.getOwnPropertyDescriptor(HTMLElement.prototype, prop);
    Object.defineProperty(HTMLElement.prototype, prop, { configurable: true, value });
  };
  afterEach(() => {
    for (const [prop, descriptor] of Object.entries(saved)) {
      if (descriptor) Object.defineProperty(HTMLElement.prototype, prop, descriptor);
      else delete (HTMLElement.prototype as unknown as Record<string, unknown>)[prop];
    }
  });

  const many = (n: number) =>
    Array.from({ length: n }, (_, i) => item({ id: `s${i}`, name: `svc-${i}` }));

  it("draws every row up to 100", async () => {
    lay("offsetHeight", 400);
    lay("offsetWidth", 800);
    answer = () => Response.json(body(many(100)));
    mount();
    await table();
    expect(bodyRows()).toHaveLength(100);
    expect(screen.getByText("100 failing services and containers")).toBeTruthy();
  });

  it("draws only the rows in view over 100, inside a bounded scroll area, and counts them all", async () => {
    lay("offsetHeight", 400);
    lay("offsetWidth", 800);
    answer = () => Response.json(body(many(300)));
    mount();
    const t = await table();
    await waitFor(() =>
      expect(screen.getByText("300 failing services and containers")).toBeTruthy(),
    );
    const drawn = bodyRows().filter(
      (r) => within(r as HTMLElement).queryAllByRole("cell").length > 1,
    );
    expect(drawn.length).toBeGreaterThan(0);
    expect(drawn.length).toBeLessThan(100);
    expect(t.parentElement?.className).toContain("max-h-[70svh]");
  });
});
