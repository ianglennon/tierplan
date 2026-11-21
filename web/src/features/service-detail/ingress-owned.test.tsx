// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { queryClient } from "@/api/query-client";
import { serviceRetry } from "@/api/services";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { ServiceAlerts } from "@/features/service-detail/alerts";
import { routeTree } from "@/routeTree.gen";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";
import {
  bareServiceDetail,
  rollout,
  SERVICE_ENV,
  SERVICE_ID,
  type ServiceApiState,
  type ServiceDetailBody,
  serviceDetail,
  serviceFetch,
} from "@/test-service";

// The ingress proxy's service page: the badge in the header (UI-SPEC S3, D-29) and the neutral
// read-only notice under it (U-03).

const PAGE = `/env/${SERVICE_ENV}/services/${SERVICE_ID}`;
const TITLE = "Managed by TierPlan ingress";
const BODY =
  "TierPlan creates and updates this itself. To remove it, switch ingress off. You can still read its logs and inspect it.";

let state: ServiceApiState;
const defaults = queryClient.getDefaultOptions();

beforeEach(() => {
  state = {
    sent: [],
    me: makeMe({ envs: { [SERVICE_ENV]: "operator" } }),
    detail: () => Response.json(serviceDetail()),
    inspect: () => Response.json({ masked: true, document: { ID: SERVICE_ID } }),
    scale: () => Response.json({ results: [] }),
  };
  queryClient.setDefaultOptions({ queries: { retry: false, refetchOnWindowFocus: false } });
  serviceRetry.attempts = 0;
  setCsrfToken(null);
  FakeEventSource.reset();
  vi.stubGlobal("EventSource", FakeEventSource);
  vi.stubGlobal("fetch", vi.fn(serviceFetch(state)));
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  queryClient.clear();
  queryClient.setDefaultOptions(defaults);
  serviceRetry.attempts = 1;
  document.title = "";
});

function renderPage() {
  const router = createRouter({
    routeTree,
    history: createMemoryHistory({ initialEntries: [PAGE] }),
  });
  render(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
}

function headerOf(): HTMLElement {
  const header = screen.getByRole("heading", { level: 2 }).closest("header");
  if (!header) throw new Error("no header");
  return header;
}

describe("the service header of the ingress proxy", () => {
  it("draws the TierPlan ingress badge with its lock", async () => {
    state.detail = () => Response.json(serviceDetail({ ingress: true, name: "tierplan-ingress" }));
    renderPage();
    await screen.findByRole("heading", { name: "tierplan-ingress" });
    const badge = within(headerOf()).getByText("TierPlan ingress");
    expect(badge.querySelector("svg")?.getAttribute("aria-hidden")).toBe("true");
    expect(badge.className).toContain("shrink-0");
  });

  it("draws no badge for a plain service", async () => {
    renderPage();
    await screen.findByRole("heading", { name: "api" });
    expect(within(headerOf()).queryByText("TierPlan ingress")).toBeNull();
  });
});

// mount puts only the alerts on the page, as alerts.test.tsx does.
async function mount(detail: ServiceDetailBody) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === `/api/env/${SERVICE_ENV}/services/${SERVICE_ID}/rollout`) {
        return Response.json(rollout());
      }
      return Response.json({}, { status: 404 });
    }),
  );
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <TooltipProvider>
        <TestRouter>
          <ServiceAlerts envId={SERVICE_ENV} detail={detail} />
          <Toaster />
        </TestRouter>
      </TooltipProvider>
    </QueryClientProvider>,
  );
  await waitFor(() => expect(fetch).toHaveBeenCalled());
}

describe("the read-only notice of the ingress proxy", () => {
  it("is a neutral status alert with the title and the body, and a hidden lock", async () => {
    await mount(bareServiceDetail({ ingress: true, running: 3, desired: 3 }));
    const title = await screen.findByText(TITLE);
    const alert = title.closest('[data-slot="alert"]') as HTMLElement;
    expect(alert).toBeTruthy();
    expect(within(alert).getByText(BODY)).toBeTruthy();
    expect(alert.querySelector("svg")?.getAttribute("aria-hidden")).toBe("true");
    // Plain status: not announced as an alert, and not the warning treatment.
    expect(alert.getAttribute("role")).not.toBe("alert");
    expect(screen.queryByRole("alert")).toBeNull();
    expect(alert.className).not.toContain("status-warning");
    expect(alert.className).toContain("bg-card");
  });

  it("leaves the agent service its warning and shows no ingress notice there", async () => {
    await mount(bareServiceDetail({ agent: true, running: 3, desired: 3 }));
    expect(await screen.findByText(/This is TierPlan's agent service/)).toBeTruthy();
    expect(screen.queryByText(TITLE)).toBeNull();
  });

  it("shows nothing for a plain service", async () => {
    await mount(bareServiceDetail({ running: 3, desired: 3 }));
    expect(screen.queryByText(TITLE)).toBeNull();
    expect(screen.queryByText(/agent service/)).toBeNull();
  });
});
