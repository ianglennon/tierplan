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
let client: QueryClient;

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

beforeEach(() => {
  setCsrfToken(null);
  serve([]);
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(makeMe({ envs: { [ENV]: "viewer" } }));
      if (path === `/api/env/${ENV}/ingress/routes`) return answer();
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
    ).toEqual(["Route", "Stack", "Status", "Conditions"]);
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
});
