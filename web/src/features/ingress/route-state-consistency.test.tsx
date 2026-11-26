// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { RoutesTab } from "@/features/ingress/routes-tab";
import { StackRoutes } from "@/features/stacks/stack-routes";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// One route, drawn by the stack page's Routes card and by the Ingress page's Routes tab, must read the same in both
// (A-03): the same label and the same dot. Both pages take their answer from the one fixture below, so a difference
// on screen is a difference in how they draw it.

const ENV = "e1";

type Json = Record<string, unknown>;

// The route as the server answers it for both pages: the service web is off the ingress network and runs no
// replica, so the state is backend_not_running and its two conditions are not ready.
function fixture(stopped: boolean): Json {
  return {
    hostname: "app.example.com",
    path: "/",
    reason: "",
    service: "web",
    state: "backend_not_running",
    stopped,
  };
}

function conditions(): Json[] {
  const states: Record<string, string> = {
    network_attached: "not_ready",
    backend_healthy: "not_ready",
  };
  return ["claim_held", "network_attached", "backend_healthy", "certificate_ready", "applied"].map(
    (code) => ({ code, state: states[code] ?? "ready", reason: "", message: "" }),
  );
}

let stopped: boolean;
let client: QueryClient;

beforeEach(() => {
  setCsrfToken(null);
  stopped = false;
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(makeMe({ envs: { [ENV]: "viewer" } }));
      if (path === `/api/env/${ENV}/stacks/s1/routes`) {
        return Response.json({ routes: [fixture(stopped)] });
      }
      if (path === `/api/env/${ENV}/ingress/routes`) {
        return Response.json({
          enabled: true,
          routes: [
            {
              ...fixture(stopped),
              kind: "route",
              ownerKind: "stack",
              port: 8080,
              scheme: "http",
              stackId: "s1",
              stackName: "shop",
              strip: false,
              conditions: conditions(),
            },
          ],
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

// Both pages, one above the other, each in its own query cache entry but the same page.
async function bothPages() {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <section data-testid="stack-page">
            <StackRoutes envId={ENV} stack={{ id: "s1", stopped, target: "swarm" }} />
          </section>
          <section data-testid="ingress-page">
            <RoutesTab envId={ENV} />
          </section>
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
  const stackPage = await screen.findByTestId("stack-page");
  const ingressPage = await screen.findByTestId("ingress-page");
  await waitFor(() => expect(within(stackPage).getByText("app.example.com")).toBeTruthy());
  await waitFor(() => expect(within(ingressPage).getByText("app.example.com")).toBeTruthy());
  const rowIn = (page: HTMLElement) => {
    const row = within(page).getByText("app.example.com").closest("tr");
    if (!row) throw new Error("no row");
    return row as HTMLElement;
  };
  return { stackRow: rowIn(stackPage), ingressRow: rowIn(ingressPage) };
}

// The one dot of a row's headline: the status dot beside the label, hidden from the accessibility tree.
function dotClass(row: HTMLElement): string {
  const label = within(row).getByText(/Backend not running/);
  const dot = label.parentElement?.querySelector("span[aria-hidden='true']");
  if (!dot) throw new Error("no dot beside the label");
  return dot.className;
}

describe("a route on the stack page and on the Ingress page", () => {
  it("is Backend not running with a red dot on both when its stack runs and the service has no replica", async () => {
    const { stackRow, ingressRow } = await bothPages();
    expect(within(stackRow).getByText("Backend not running")).toBeTruthy();
    expect(within(ingressRow).getByText("Backend not running")).toBeTruthy();
    expect(dotClass(stackRow)).toContain("bg-status-offline");
    expect(dotClass(ingressRow)).toContain("bg-status-offline");
    expect(dotClass(ingressRow)).toBe(dotClass(stackRow));
    // The Ingress page adds the conditions behind the headline; the headline itself is the same.
    expect(within(ingressRow).getByText("3 of 5 ready")).toBeTruthy();
  });

  it("is Backend not running with a grey dot on both when the stack is stopped", async () => {
    stopped = true;
    const { stackRow, ingressRow } = await bothPages();
    expect(within(stackRow).getByText("Backend not running")).toBeTruthy();
    expect(within(ingressRow).getByText("Backend not running")).toBeTruthy();
    expect(dotClass(stackRow)).toContain("bg-status-unknown");
    expect(dotClass(ingressRow)).toContain("bg-status-unknown");
    expect(dotClass(ingressRow)).toBe(dotClass(stackRow));
    expect(dotClass(stackRow)).not.toContain("bg-status-offline");
  });
});
