// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { RoutesTab } from "@/features/ingress/routes-tab";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

const ENV = "e1";

type Json = Record<string, unknown>;

let answer: () => Response;
let certs: () => Response;
let client: QueryClient;
let swarm: boolean;
let me: ReturnType<typeof makeMe>;

function condition(code: string, state = "ready", over: Json = {}): Json {
  return { code, state, reason: "", message: "", ...over };
}

function conditions(states: Record<string, string> = {}): Json[] {
  return ["claim_held", "network_attached", "backend_healthy", "certificate_ready", "applied"].map(
    (code) => condition(code, states[code] ?? "ready"),
  );
}

function route(over: Json = {}): Json {
  return {
    kind: "route",
    hostname: "app.example.com",
    path: "/",
    ownerKind: "stack",
    port: 8080,
    reason: "",
    scheme: "http",
    service: "web",
    stackId: "s1",
    stackName: "shop",
    state: "serving",
    stopped: false,
    strip: false,
    conditions: conditions(),
    ...over,
  };
}

function serve(routes: Json[]) {
  answer = () => Response.json({ enabled: true, routes });
}

function serveCerts(...hostnames: string[]) {
  certs = () =>
    Response.json({ items: hostnames.map((hostname, i) => ({ id: `c${i}`, hostname })) });
}

beforeEach(() => {
  setCsrfToken(null);
  serve([]);
  certs = () => Response.json({ items: [] });
  swarm = false;
  me = makeMe({ envs: { [ENV]: "viewer" } });
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(me);
      if (path === `/api/env/${ENV}/ingress/routes`) return answer();
      if (path === `/api/env/${ENV}/ingress/certificates`) return certs();
      if (path === "/api/environments") {
        return Response.json({
          items: [{ id: ENV, name: "Lab", envType: swarm ? "swarm" : "standalone", enabled: true }],
          total: 1,
        });
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  client.clear();
});

function mount() {
  return render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <RoutesTab envId={ENV} />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

async function table() {
  return screen.findByRole("table", { name: "Routes" });
}

function item(items: HTMLElement[], i: number): HTMLElement {
  const found = items[i];
  if (!found) throw new Error(`no item ${i}`);
  return found;
}

const rowOf = (t: HTMLElement, host: string) =>
  within(t).getByText(host).closest("tr") as HTMLElement;

describe("the Ingress page's Routes tab", () => {
  it("draws each route's hostname, target, stack, headline and ready count", async () => {
    serve([
      route(),
      route({
        hostname: "old.example.com",
        path: "/api",
        service: "api",
        stackId: "s2",
        stackName: "legacy",
        state: "not_attached",
        conditions: conditions({ network_attached: "not_ready" }),
      }),
    ]);
    mount();
    const t = await table();
    expect(
      within(t)
        .getAllByRole("columnheader")
        .map((h) => h.textContent),
    ).toEqual(["Route", "Stack", "Status", "Conditions", "Certificate"]);
    const serving = within(t).getByText("app.example.com");
    expect(serving.className).toContain("font-mono");
    expect(serving.className).toContain("break-all");
    const first = rowOf(t, "app.example.com");
    expect(within(first).getByText("http to port 8080")).toBeTruthy();
    expect(within(first).getByText("shop")).toBeTruthy();
    expect(within(first).getByText("Serving")).toBeTruthy();
    expect(within(first).getByText("5 of 5 ready")).toBeTruthy();
    const second = rowOf(t, "old.example.com/api");
    expect(within(second).getByText("Not attached")).toBeTruthy();
    expect(within(second).getByText("4 of 5 ready")).toBeTruthy();
    expect(within(second).getByText("api").className).toContain("font-mono");
  });

  it("draws a stopped stack's backend as grey and a running stack's as red, as the stack page does", async () => {
    serve([
      route({ hostname: "a.example.com", state: "backend_not_running", stopped: true }),
      route({ hostname: "b.example.com", state: "backend_not_running", stopped: false }),
    ]);
    mount();
    const t = await table();
    const dot = (host: string) =>
      rowOf(t, host).querySelector("span[aria-hidden='true']") as HTMLElement;
    expect(dot("a.example.com").className).toContain("bg-status-unknown");
    expect(dot("b.example.com").className).toContain("bg-status-offline");
  });

  it("says the routes are loading and then reads a failed answer with a retry", async () => {
    answer = () => Response.json({ title: "boom", status: 500 }, { status: 500 });
    mount();
    expect(screen.getByText("Loading routes")).toBeTruthy();
    await waitFor(() => expect(screen.getByText("Couldn't read the routes.")).toBeTruthy());
    expect(screen.getByRole("button", { name: "Retry reading routes" })).toBeTruthy();
  });

  it("opens a route's five conditions in order, each with its word and reason", async () => {
    serve([
      route({
        hostname: "app.example.com",
        service: "web",
        state: "not_attached",
        conditions: [
          condition("claim_held"),
          condition("network_attached", "not_ready", { reason: "not_attached" }),
          condition("backend_healthy"),
          condition("certificate_ready", "not_ready", {
            reason: "certificate_not_ready",
            message: "Waiting for the DNS record to appear.",
          }),
          { ...condition("applied"), applied: { nodes: 2, total: 2 } },
        ],
      }),
    ]);
    mount();
    const t = await table();
    const button = within(t).getByRole("button", { name: "Show conditions for app.example.com" });
    expect(button.getAttribute("aria-expanded")).toBe("false");
    expect(within(t).queryByText("Claim held")).toBeNull();
    fireEvent.click(button);
    expect(button.getAttribute("aria-expanded")).toBe("true");
    const list = within(t).getByRole("list", { name: "Conditions of app.example.com" });
    const items = within(list).getAllByRole("listitem");
    expect(
      items.map(
        (i) => within(i).getByText(/^(Claim|Network|Backend|Certificate|Applied)/).textContent,
      ),
    ).toEqual([
      "Claim held",
      "Network attached",
      "Backend healthy",
      "Certificate ready",
      "Applied on nodes",
    ]);
    expect(within(item(items, 0)).getByText("Ready")).toBeTruthy();
    expect(within(item(items, 1)).getByText("Not ready")).toBeTruthy();
    expect(
      within(item(items, 1)).getByText(
        "web isn't on the ingress network. Redeploy the stack to attach it.",
      ),
    ).toBeTruthy();
    expect(within(item(items, 3)).getByText("Waiting for the DNS record to appear.")).toBeTruthy();
    expect(within(item(items, 4)).getByText("On 2 of 2 nodes.")).toBeTruthy();
  });

  it("falls back to the server's message for a reason code it has no words for", async () => {
    serve([
      route({
        conditions: [
          condition("claim_held"),
          condition("network_attached"),
          condition("backend_healthy", "not_ready", {
            reason: "something_new",
            message: "The server says so.",
          }),
          condition("certificate_ready"),
          condition("applied"),
        ],
      }),
    ]);
    mount();
    const t = await table();
    fireEvent.click(within(t).getByRole("button", { name: /Show conditions for/ }));
    expect(within(t).getByText("The server says so.")).toBeTruthy();
  });

  it("never draws an unread condition as Ready: it is Unknown with a grey dot", async () => {
    const unread = (code: string) =>
      condition(code, "unknown", { reason: "engine_unreadable", message: "engine down" });
    serve([
      route({
        state: "unknown",
        conditions: [
          "claim_held",
          "network_attached",
          "backend_healthy",
          "certificate_ready",
          "applied",
        ].map(unread),
      }),
    ]);
    mount();
    const t = await table();
    expect(within(t).getByText("0 of 5 ready")).toBeTruthy();
    fireEvent.click(within(t).getByRole("button", { name: /Show conditions for/ }));
    const list = within(t).getByRole("list", { name: /Conditions of/ });
    expect(within(list).queryByText("Ready")).toBeNull();
    const words = within(list).getAllByText("Unknown");
    expect(words).toHaveLength(5);
    for (const w of words) {
      const dot = w.previousElementSibling as HTMLElement;
      expect(dot.className).toContain("bg-status-unknown");
      expect(dot.className).not.toContain("bg-status-online");
    }
    expect(
      within(list).getAllByText("TierPlan couldn't read this environment just now."),
    ).toHaveLength(5);
  });

  it("draws a stopped stack's backend grey and any other not-ready backend red", async () => {
    const backend = (reason: string) => [
      condition("claim_held"),
      condition("network_attached"),
      condition("backend_healthy", "not_ready", { reason }),
      condition("certificate_ready"),
      condition("applied"),
    ];
    serve([
      route({ hostname: "a.example.com", conditions: backend("stack_stopped") }),
      route({ hostname: "b.example.com", conditions: backend("no_running_task") }),
    ]);
    mount();
    const t = await table();
    for (const host of ["a.example.com", "b.example.com"]) {
      fireEvent.click(within(t).getByRole("button", { name: `Show conditions for ${host}` }));
    }
    const dotOf = (host: string) => {
      const list = within(t).getByRole("list", { name: `Conditions of ${host}` });
      const item = within(list).getAllByRole("listitem")[2] as HTMLElement;
      return within(item).getByText("Not ready").previousElementSibling as HTMLElement;
    };
    expect(dotOf("a.example.com").className).toContain("bg-status-unknown");
    expect(dotOf("b.example.com").className).toContain("bg-status-offline");
  });

  it("keeps a route open across a refetch and carries the motion classes", async () => {
    serve([route({ state: "pending", conditions: conditions({ applied: "not_ready" }) })]);
    mount();
    const t = await table();
    const button = within(t).getByRole("button", { name: "Show conditions for app.example.com" });
    expect(button.querySelector("svg")?.getAttribute("class")).toContain(
      "motion-safe:transition-transform",
    );
    fireEvent.click(button);
    expect(within(t).getByText("Applied on nodes")).toBeTruthy();
    serve([
      route({
        state: "pending",
        conditions: conditions({ applied: "not_ready", backend_healthy: "ready" }),
      }),
    ]);
    await client.refetchQueries({ queryKey: ["env", ENV, "ingress", "routes"] });
    const again = within(await table()).getByRole("button", {
      name: "Show conditions for app.example.com",
    });
    expect(again.getAttribute("aria-expanded")).toBe("true");
    expect(within(t).getByText("Applied on nodes")).toBeTruthy();
    // A route that is pending pulses its dot, and the pulse stops for a reader who asks for less motion.
    const pending = within(t).getByText("Rolling out").previousElementSibling as HTMLElement;
    expect(pending.className).toContain("animate-pulse");
    expect(pending.className).toContain("motion-reduce:animate-none");
  });

  describe("held and reserved names, the filter and the summary (D-08, D-25, U-16)", () => {
    const claim = (over: Json = {}): Json => ({
      kind: "claim",
      hostname: "held.example.com",
      path: "",
      ownerKind: "stack",
      port: 0,
      reason: "",
      scheme: "",
      service: "",
      stackId: "s9",
      stackName: "parked",
      state: "",
      stopped: false,
      strip: false,
      conditions: [],
      ...over,
    });
    const reserved = (over: Json = {}): Json => ({
      ...claim(),
      kind: "reserved",
      hostname: "kept.example.com",
      ownerKind: "",
      stackId: "",
      stackName: "",
      ...over,
    });
    const mixed = (): Json[] => [
      route({ hostname: "a.example.com", stackName: "shop", service: "web" }),
      route({ hostname: "b.example.com", stackName: "blog", service: "web" }),
      route({
        hostname: "c.example.com",
        stackName: "wiki",
        service: "app",
        state: "not_attached",
        conditions: conditions({ network_attached: "not_ready" }),
      }),
      route({
        hostname: "d.example.com",
        stackName: "old",
        service: "app",
        state: "backend_not_running",
        stopped: true,
      }),
      claim(),
      reserved(),
    ];

    it("draws the claim and reserved rows after the routes, with their words and no conditions", async () => {
      serve(mixed());
      mount();
      const t = await table();
      const hosts = within(t)
        .getAllByRole("row")
        .slice(1)
        .map((r) => (r.querySelector("span.font-mono") as HTMLElement).textContent);
      expect(hosts).toEqual([
        "a.example.com",
        "b.example.com",
        "c.example.com",
        "d.example.com",
        "held.example.com",
        "kept.example.com",
      ]);
      const held = rowOf(t, "held.example.com");
      expect(within(held).getByText("Claim only")).toBeTruthy();
      expect(within(held).getByText("No route")).toBeTruthy();
      expect(
        within(held).getByText("A stack holds this name but none of its services serves it."),
      ).toBeTruthy();
      expect(within(held).getByRole("link", { name: "parked" })).toBeTruthy();
      // Conditions and Certificate both say nothing.
      expect(within(held).getAllByText("—")).toHaveLength(2);
      expect(within(held).queryByRole("button")).toBeNull();
      const kept = rowOf(t, "kept.example.com");
      expect(within(kept).getByText("Reserved", { selector: "[data-slot=badge]" })).toBeTruthy();
      expect(within(kept).getAllByText("Reserved")).toHaveLength(2);
      expect(
        within(kept).getByText("Held back on the Domains tab. No stack can claim it."),
      ).toBeTruthy();
      // The stack column, the Conditions column and the Certificate column all say nothing.
      expect(within(kept).getAllByText("—")).toHaveLength(3);
      expect(within(kept).queryByRole("button")).toBeNull();
      expect(within(kept).queryByRole("link")).toBeNull();
    });

    it("always writes the summary line, zero counts included", async () => {
      serve(mixed());
      const first = mount();
      expect(
        await screen.findByText("4 routes: 2 serving, 1 need attention, 1 stopped."),
      ).toBeTruthy();
      first.unmount();
      serve([]);
      client.clear();
      mount();
      expect(
        await screen.findByText("0 routes: 0 serving, 0 need attention, 0 stopped."),
      ).toBeTruthy();
    });

    it("says route in the singular for one", async () => {
      serve([route()]);
      mount();
      expect(
        await screen.findByText("1 route: 1 serving, 0 need attention, 0 stopped."),
      ).toBeTruthy();
    });

    it("filters on hostname, path, stack and service in any case, before anything is cut", async () => {
      serve(mixed());
      mount();
      const t = await table();
      const filter = screen.getByRole("textbox", { name: "Filter routes" });
      expect(filter.getAttribute("placeholder")).toBe("Filter by hostname, stack or service");
      fireEvent.change(filter, { target: { value: "SHOP" } });
      expect(within(t).getByText("a.example.com")).toBeTruthy();
      expect(within(t).queryByText("b.example.com")).toBeNull();
      fireEvent.change(filter, { target: { value: "APP" } });
      expect(within(t).queryByText("a.example.com")).toBeNull();
      expect(within(t).getByText("c.example.com")).toBeTruthy();
      expect(within(t).getByText("d.example.com")).toBeTruthy();
      fireEvent.change(filter, { target: { value: "b.EXAMPLE" } });
      expect(within(t).getByText("b.example.com")).toBeTruthy();
      expect(within(t).queryByText("c.example.com")).toBeNull();
    });

    it("matches a path", async () => {
      serve([
        route({ hostname: "a.example.com", path: "/Api" }),
        route({ hostname: "b.example.com" }),
      ]);
      mount();
      const t = await table();
      fireEvent.change(screen.getByRole("textbox", { name: "Filter routes" }), {
        target: { value: "/api" },
      });
      expect(within(t).getByText("a.example.com/Api")).toBeTruthy();
      expect(within(t).queryByText("b.example.com")).toBeNull();
    });

    it("Needs attention leaves only the route that is neither serving, stopped nor off", async () => {
      serve(mixed());
      mount();
      const t = await table();
      fireEvent.click(screen.getByRole("radio", { name: "Needs attention" }));
      const rows = within(t).getAllByRole("row").slice(1);
      expect(rows).toHaveLength(1);
      expect(within(t).getByText("c.example.com")).toBeTruthy();
      fireEvent.click(screen.getByRole("radio", { name: "All" }));
      expect(within(t).getAllByRole("row").slice(1)).toHaveLength(6);
    });

    it("hides a route that is off from Needs attention", async () => {
      serve([
        route({ hostname: "a.example.com", state: "ingress_off" }),
        route({ hostname: "b.example.com", state: "unknown" }),
      ]);
      mount();
      const t = await table();
      fireEvent.click(screen.getByRole("radio", { name: "Needs attention" }));
      expect(within(t).queryByText("a.example.com")).toBeNull();
      expect(within(t).getByText("b.example.com")).toBeTruthy();
    });
  });

  describe("show ten, links, proxy logs and every state (D-11, D-03, S2, S8)", () => {
    const many = (n: number): Json[] =>
      Array.from({ length: n }, (_, i) =>
        route({ hostname: `h${String(i).padStart(2, "0")}.example.com` }),
      );
    const bodyRows = (t: HTMLElement) => within(t).getAllByRole("row").slice(1);

    it("shows the first ten of twelve, then all of them, then ten again", async () => {
      serve(many(12));
      mount();
      const t = await table();
      expect(bodyRows(t)).toHaveLength(10);
      const more = screen.getByRole("button", { name: "Show all 12" });
      fireEvent.click(more);
      expect(bodyRows(t)).toHaveLength(12);
      fireEvent.click(screen.getByRole("button", { name: "Show fewer" }));
      expect(bodyRows(t)).toHaveLength(10);
    });

    it("cuts the filtered list, not the whole one", async () => {
      serve([...many(12), route({ hostname: "odd.example.com" })]);
      mount();
      const t = await table();
      fireEvent.change(screen.getByRole("textbox", { name: "Filter routes" }), {
        target: { value: "h0" },
      });
      expect(bodyRows(t)).toHaveLength(10);
      expect(screen.queryByRole("button", { name: /Show all/ })).toBeNull();
    });

    it("links the stack to its page and draws the service as text on a standalone host", async () => {
      serve([route()]);
      mount();
      const t = await table();
      expect(within(t).getByRole("link", { name: "shop" }).getAttribute("href")).toBe(
        "/env/e1/stacks/s1",
      );
      expect(within(t).queryByRole("link", { name: "web" })).toBeNull();
      expect(within(t).getByText("web")).toBeTruthy();
    });

    it("links the service to its page on a swarm, by its swarm name", async () => {
      swarm = true;
      serve([route()]);
      mount();
      const t = await table();
      const link = await within(t).findByRole("link", { name: "web" });
      expect(link.getAttribute("href")).toBe("/env/e1/services/shop_web");
    });

    it("links to the proxy service's logs on a swarm for a reader of services", async () => {
      swarm = true;
      serve([route()]);
      mount();
      const link = await screen.findByRole("link", { name: "View proxy logs" });
      expect(link.getAttribute("href")).toBe("/env/e1/services/tierplan-ingress/logs");
    });

    it("links to the proxy container's logs on a standalone host", async () => {
      serve([route()]);
      mount();
      const link = await screen.findByRole("link", { name: "View proxy logs" });
      expect(link.getAttribute("href")).toBe("/env/e1/containers/tierplan-ingress-proxy-1/logs");
    });

    it("does not draw the proxy logs link to someone who may not read them", async () => {
      swarm = true;
      me = makeMe({ permissions: { global: [], environments: { [ENV]: ["ingress.read"] } } });
      serve([route()]);
      mount();
      await table();
      await waitFor(() =>
        expect(within(document.body).getAllByRole("link").length).toBeGreaterThan(0),
      );
      expect(screen.queryByRole("link", { name: "View proxy logs" })).toBeNull();
    });

    it("says ingress is off, and tells a reader to ask an Admin", async () => {
      answer = () => Response.json({ enabled: false, routes: [] });
      mount();
      expect(await screen.findByText("Ingress is off")).toBeTruthy();
      expect(
        screen.getByText(/Ingress serves your stacks' hostnames over HTTPS, with a certificate/),
      ).toBeTruthy();
      expect(screen.getByText(/Ask an Admin to turn it on\./)).toBeTruthy();
      expect(screen.queryByRole("table")).toBeNull();
    });

    it("does not tell an Admin to ask an Admin", async () => {
      me = makeMe({ envs: { [ENV]: "admin" } });
      answer = () => Response.json({ enabled: false, routes: [] });
      mount();
      expect(await screen.findByText("Ingress is off")).toBeTruthy();
      expect(screen.queryByText(/Ask an Admin/)).toBeNull();
    });

    it("says there are no routes yet when ingress is on and nothing is held", async () => {
      serve([]);
      mount();
      expect(await screen.findByText("No routes yet")).toBeTruthy();
      expect(
        screen.getByText(
          "A route appears when a stack has tierplan.ingress.* labels and is deployed. The Expose panel in the stack editor writes them.",
        ),
      ).toBeTruthy();
      expect(screen.queryByRole("table")).toBeNull();
    });

    it("says no route matches a filter, and Clear filter brings every row back", async () => {
      serve(many(3));
      mount();
      const t = await table();
      fireEvent.change(screen.getByRole("textbox", { name: "Filter routes" }), {
        target: { value: "zzz" },
      });
      expect(screen.getByText("No route matches “zzz”.")).toBeTruthy();
      fireEvent.click(screen.getByRole("button", { name: "Clear filter" }));
      expect(bodyRows(t)).toHaveLength(3);
      expect(
        (screen.getByRole("textbox", { name: "Filter routes" }) as HTMLInputElement).value,
      ).toBe("");
    });

    it("refetches when Retry reading routes is pressed after a failed read", async () => {
      answer = () => Response.json({ title: "boom", status: 500 }, { status: 500 });
      mount();
      const retry = await screen.findByRole("button", { name: "Retry reading routes" });
      serve([route()]);
      fireEvent.click(retry);
      expect(await table()).toBeTruthy();
    });

    it("draws a route whose conditions are unknown with a grey Unknown headline and no error", async () => {
      const unread = (code: string) =>
        condition(code, "unknown", { reason: "engine_unreadable", message: "engine down" });
      serve([
        route({
          state: "unknown",
          conditions: [
            "claim_held",
            "network_attached",
            "backend_healthy",
            "certificate_ready",
            "applied",
          ].map(unread),
        }),
      ]);
      mount();
      const t = await table();
      const row = rowOf(t, "app.example.com");
      const word = within(row).getByText("Unknown");
      expect((word.previousElementSibling as HTMLElement).className).toContain("bg-status-unknown");
      expect(
        within(row).getByText("TierPlan couldn't read this environment just now."),
      ).toBeTruthy();
      expect(screen.queryByRole("alert")).toBeNull();
    });
  });

  describe("the Certificate column", () => {
    it("links a route whose hostname has a certificate to it, and shows a dash otherwise", async () => {
      serve([
        route(),
        route({ hostname: "bare.example.com", service: "api", stackName: "legacy", stackId: "s2" }),
      ]);
      serveCerts("app.example.com", "other.example.com");
      mount();
      const t = await table();
      const link = await within(rowOf(t, "app.example.com")).findByRole("link", {
        name: "View certificate: app.example.com",
      });
      expect(link.textContent).toBe("View certificate");
      expect(link.getAttribute("href")).toBe(
        `/env/${ENV}/ingress/certificates?host=app.example.com`,
      );
      const bare = rowOf(t, "bare.example.com");
      expect(within(bare).queryByRole("link", { name: /View certificate/ })).toBeNull();
      expect(within(bare).getByText("—")).toBeTruthy();
    });

    it("compares hostnames without regard to case and ignores the path", async () => {
      serve([route({ hostname: "App.Example.com", path: "/api" })]);
      serveCerts("app.example.com");
      mount();
      const t = await table();
      expect(
        await within(t).findByRole("link", { name: "View certificate: App.Example.com/api" }),
      ).toBeTruthy();
    });

    it("shows a dash while the certificates cannot be read", async () => {
      serve([route()]);
      certs = () => Response.json({ title: "boom" }, { status: 500 });
      mount();
      const t = await table();
      await waitFor(() =>
        expect(within(t).queryByRole("link", { name: /View certificate/ })).toBeNull(),
      );
      expect(within(rowOf(t, "app.example.com")).getByText("—")).toBeTruthy();
    });
  });
});
