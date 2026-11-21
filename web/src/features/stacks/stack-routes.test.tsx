// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { LIST_REFETCH_MS } from "@/api/resources";
import { stackRoutesQuery } from "@/api/stack-routes";
import { TooltipProvider } from "@/components/ui/tooltip";
import { StackRoutes } from "@/features/stacks/stack-routes";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

const ENV = "e1";

type Json = Record<string, unknown>;

let role: "viewer" | "operator" | "admin";
let answer: () => Response;
let calls: number;
let client: QueryClient;

function route(over: Json = {}): Json {
  return {
    hostname: "app.example.com",
    path: "/",
    reason: "",
    service: "web",
    state: "serving",
    stopped: false,
    ...over,
  };
}

function serve(routes: Json[]) {
  answer = () => Response.json({ routes });
}

beforeEach(() => {
  setCsrfToken(null);
  role = "operator";
  calls = 0;
  serve([]);
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(makeMe({ envs: { [ENV]: role } }));
      if (path === `/api/env/${ENV}/stacks/s1/routes`) {
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
  client.clear();
});

function mount(stopped = false) {
  return render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <StackRoutes envId={ENV} stack={{ id: "s1", stopped }} />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

async function card() {
  const c = await screen.findByRole("region", { name: "Routes" });
  await waitFor(() => expect(client.getQueryState(["me"])?.status).toBe("success"));
  return c;
}

const rowOf = (c: HTMLElement, host: string) => {
  const cell = within(c).getByText(host);
  return cell.closest("tr") as HTMLElement;
};

describe("the stack's Routes card", () => {
  it("shows a table of Route, Service and State, the hostname and path joined and no path drawing the host alone", async () => {
    serve([
      route({ hostname: "app.example.com", path: "/api", service: "api" }),
      route({ hostname: "www.example.com", path: "/", service: "web" }),
    ]);
    mount();
    const c = await card();
    const table = within(c).getByRole("table", { name: "Routes" });
    expect(
      within(table)
        .getAllByRole("columnheader")
        .map((h) => h.textContent),
    ).toEqual(["Route", "Service", "State"]);
    const joined = within(table).getByText("app.example.com/api");
    expect(joined.className).toContain("font-mono");
    expect(joined.className).toContain("break-all");
    expect(within(table).getByText("www.example.com")).toBeTruthy();
    expect(within(table).queryByText("www.example.com/")).toBeNull();
    expect(within(table).getByText("api").className).toContain("font-mono");
    expect(c.querySelector("a")).toBeNull();
  });

  it.each([
    ["serving", {}, "Serving", null, "bg-status-online"],
    ["pending", {}, "Rolling out", "Not every node has it yet.", "bg-status-warning"],
    [
      "waiting_certificate",
      { reason: "no_certificate" },
      "Waiting for certificate",
      "No certificate has been issued for this hostname yet.",
      "bg-status-warning",
    ],
    [
      "not_attached",
      {},
      "Not attached",
      "Redeploy the stack to attach it to ingress.",
      "bg-status-warning",
    ],
    [
      "ingress_off",
      {},
      "Ingress is off",
      "It serves once ingress is switched on and the stack is redeployed.",
      "border-status-unknown",
    ],
    [
      "not_served_yet",
      {},
      "Not served yet",
      "Not served on this host yet. The stack keeps its hostnames.",
      "border-status-unknown",
    ],
  ])("draws %s with its dot, label and detail", async (state, extra, label, detail, dot) => {
    serve([route({ state, ...extra })]);
    mount();
    const c = await card();
    const row = rowOf(c, "app.example.com");
    expect(within(row).getByText(label)).toBeTruthy();
    if (detail) expect(within(row).getByText(detail)).toBeTruthy();
    else expect(row.querySelectorAll("p")).toHaveLength(0);
    expect(row.querySelector(`[aria-hidden="true"].${dot}`)).not.toBeNull();
  });

  it("gives a waiting certificate with no reason the label alone, and an unknown reason as sent", async () => {
    serve([
      route({ hostname: "a.example.com", state: "waiting_certificate" }),
      route({ hostname: "b.example.com", state: "waiting_certificate", reason: "dns_not_ready" }),
    ]);
    mount();
    const c = await card();
    expect(rowOf(c, "a.example.com").querySelectorAll("p")).toHaveLength(0);
    expect(within(rowOf(c, "b.example.com")).getByText("dns_not_ready")).toBeTruthy();
  });

  it("says a service with no running task is down: red, with the service named", async () => {
    serve([route({ state: "backend_not_running", service: "api" })]);
    mount();
    const c = await card();
    const row = rowOf(c, "app.example.com");
    expect(within(row).getByText("Backend not running")).toBeTruthy();
    expect(within(row).getByText("api has no running task.")).toBeTruthy();
    expect(row.querySelector(".bg-status-offline")).not.toBeNull();
  });

  it("says a stopped stack once above the table, with a grey dot and no per-row reason", async () => {
    serve(
      Array.from({ length: 3 }, (_, i) =>
        route({ hostname: `h${i}.example.com`, state: "backend_not_running", stopped: true }),
      ),
    );
    mount(true);
    const c = await card();
    expect(
      within(c).getByText("This stack is stopped. Its hostnames stay reserved for it."),
    ).toBeTruthy();
    expect(within(c).queryByText("The stack is stopped.")).toBeNull();
    expect(within(c).queryByText(/has no running task/)).toBeNull();
    expect(within(c).getAllByText("Backend not running")).toHaveLength(3);
    expect(c.querySelectorAll("tbody .bg-status-unknown")).toHaveLength(3);
  });

  it("gives a stopped route its own detail when the stack-wide note is not drawn", async () => {
    serve([route({ state: "backend_not_running", stopped: true })]);
    mount(false);
    const c = await card();
    expect(within(c).getByText("The stack is stopped.")).toBeTruthy();
    expect(within(c).queryByText(/Its hostnames stay reserved/)).toBeNull();
  });

  it("points a not attached route at the review for a user who may deploy", async () => {
    serve([route({ state: "not_attached" })]);
    mount();
    const c = await card();
    expect(within(c).getByText("These routes serve after the stack is redeployed.")).toBeTruthy();
    const link = await within(c).findByRole("link", { name: "Review and deploy" });
    expect(link.getAttribute("href")).toBe(`/env/${ENV}/stacks/s1/review`);
  });

  it("says it to a viewer without the link", async () => {
    role = "viewer";
    serve([route({ state: "not_attached" })]);
    mount();
    const c = await card();
    expect(within(c).getByText("These routes serve after the stack is redeployed.")).toBeTruthy();
    expect(within(c).queryByRole("link", { name: "Review and deploy" })).toBeNull();
  });

  it("draws no note or link when every route is attached", async () => {
    serve([route()]);
    mount();
    const c = await card();
    expect(within(c).queryByText("These routes serve after the stack is redeployed.")).toBeNull();
  });

  it("draws nothing for a stack with no routes, and no title while it waits", async () => {
    serve([]);
    const { container } = mount();
    expect(container.querySelector('[aria-hidden="true"]')).not.toBeNull();
    expect(screen.queryByText("Routes")).toBeNull();
    await waitFor(() => expect(calls).toBe(1));
    await waitFor(() => expect(container.querySelector('[data-slot="skeleton"]')).toBeNull());
    expect(screen.queryByRole("region", { name: "Routes" })).toBeNull();
  });

  it("draws nothing for a null routes list", async () => {
    answer = () => Response.json({ routes: null });
    const { container } = mount();
    await waitFor(() => expect(calls).toBe(1));
    await waitFor(() => expect(container.querySelector('[data-slot="skeleton"]')).toBeNull());
    expect(screen.queryByRole("region", { name: "Routes" })).toBeNull();
  });

  it("hides the card for a 403", async () => {
    answer = () =>
      Response.json({ type: "forbidden", title: "Forbidden", status: 403 }, { status: 403 });
    const { container } = mount();
    await waitFor(() => expect(calls).toBe(1));
    await waitFor(() => expect(container.querySelector('[data-slot="skeleton"]')).toBeNull());
    expect(screen.queryByRole("region", { name: "Routes" })).toBeNull();
    expect(screen.queryByText("Couldn't read the routes.")).toBeNull();
  });

  it("says it could not read the routes inside the card and reads again on Retry", async () => {
    answer = () =>
      Response.json({ type: "env_offline", title: "Bad Gateway", status: 502 }, { status: 502 });
    mount();
    const c = await card();
    expect(within(c).getByText("Couldn't read the routes.")).toBeTruthy();
    serve([route()]);
    fireEvent.click(within(c).getByRole("button", { name: "Retry reading routes" }));
    expect(await within(c).findByText("Serving")).toBeTruthy();
    expect(within(c).queryByText("Couldn't read the routes.")).toBeNull();
  });

  it("shows ten of twelve routes, then all, then fewer", async () => {
    serve(Array.from({ length: 12 }, (_, i) => route({ hostname: `h${i}.example.com` })));
    mount();
    const c = await card();
    expect(within(c).getAllByRole("row")).toHaveLength(11);
    fireEvent.click(within(c).getByRole("button", { name: "Show all 12" }));
    expect(within(c).getAllByRole("row")).toHaveLength(13);
    fireEvent.click(within(c).getByRole("button", { name: "Show fewer" }));
    expect(within(c).getAllByRole("row")).toHaveLength(11);
  });

  it("draws no Show all for ten routes", async () => {
    serve(Array.from({ length: 10 }, (_, i) => route({ hostname: `h${i}.example.com` })));
    mount();
    const c = await card();
    expect(within(c).queryByRole("button", { name: /Show all/ })).toBeNull();
  });

  it("lets the pending dot stop pulsing for reduced motion", async () => {
    serve([route({ state: "pending" })]);
    mount();
    const c = await card();
    const dot = rowOf(c, "app.example.com").querySelector(".bg-status-warning");
    expect(dot?.className).toContain("motion-reduce:animate-none");
  });

  it("is a labelled region with the card title as a heading", async () => {
    serve([route()]);
    mount();
    const c = await card();
    expect(within(c).getByRole("heading", { name: "Routes" })).toBeTruthy();
  });
});

describe("the Routes card on narrow screens and with long values", () => {
  it("writes a 120-character hostname and path whole, wrapped, and keeps the buttons touch-sized", async () => {
    const host = `${"a".repeat(60)}.${"b".repeat(56)}.io`;
    expect(host).toHaveLength(120);
    serve([route({ hostname: host, path: "/some/long/path", state: "not_attached" })]);
    mount();
    const c = await card();
    const cell = within(c).getByText(`${host}/some/long/path`);
    expect(cell.textContent).toBe(`${host}/some/long/path`);
    expect(cell.className).toContain("break-all");
    expect(cell.className).toContain("whitespace-normal");
    const link = await within(c).findByRole("link", { name: "Review and deploy" });
    expect(link.className).toContain("max-md:h-11");
  });

  it("keeps the table in the sideways-scrolling container", async () => {
    serve([route()]);
    mount();
    const c = await card();
    const table = within(c).getByRole("table", { name: "Routes" });
    const box = table.parentElement;
    expect(box?.getAttribute("data-slot")).toBe("table-container");
    expect(box?.className).toContain("overflow-x-auto");
  });

  it("keeps the Retry and Show all buttons touch-sized", async () => {
    answer = () => Response.json({ type: "x", title: "x", status: 502 }, { status: 502 });
    mount();
    const c = await card();
    expect(within(c).getByRole("button", { name: "Retry reading routes" }).className).toContain(
      "max-md:h-11",
    );
    cleanup();
    serve(Array.from({ length: 11 }, (_, i) => route({ hostname: `h${i}.example.com` })));
    client.clear();
    mount();
    const d = await card();
    expect(within(d).getByRole("button", { name: "Show all 11" }).className).toContain(
      "max-md:h-11",
    );
  });

  it("wraps a long reason and a long service name instead of widening the card", async () => {
    const reason = "x".repeat(150);
    serve([route({ state: "waiting_certificate", reason, service: "s".repeat(90) })]);
    mount();
    const c = await card();
    const detail = within(c).getByText(reason);
    expect(detail.className).toContain("break-words");
    expect(within(c).getByText("s".repeat(90)).className).toContain("break-all");
  });
});

describe("the routes read's refresh", () => {
  const interval = (routes: Json[] | undefined) =>
    (
      stackRoutesQuery(ENV, "s1").refetchInterval as (q: {
        state: { data: unknown };
      }) => number | false
    )({ state: { data: routes === undefined ? undefined : { routes } } });

  it("is 10 s while any route is pending or waiting for a certificate", () => {
    vi.useFakeTimers();
    expect(interval([route(), route({ state: "pending" })])).toBe(10_000);
    expect(interval([route({ state: "waiting_certificate" })])).toBe(10_000);
  });

  it("follows the page's 30 s refresh otherwise", () => {
    vi.useFakeTimers();
    expect(interval([route(), route({ state: "not_attached" })])).toBe(LIST_REFETCH_MS);
    expect(interval([])).toBe(LIST_REFETCH_MS);
    expect(interval(undefined)).toBe(LIST_REFETCH_MS);
  });

  it("reads again when ten seconds pass while a route is pending, and not before", async () => {
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval", "setTimeout", "clearTimeout"] });
    serve([route({ state: "pending" })]);
    mount();
    await vi.advanceTimersByTimeAsync(0);
    expect(calls).toBe(1);
    await vi.advanceTimersByTimeAsync(9_000);
    expect(calls).toBe(1);
    await vi.advanceTimersByTimeAsync(1_500);
    expect(calls).toBe(2);
  });
});
