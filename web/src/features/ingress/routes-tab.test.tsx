// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
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
});
