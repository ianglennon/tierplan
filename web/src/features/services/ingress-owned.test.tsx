// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { components } from "@/api/schema";
import { TooltipProvider } from "@/components/ui/tooltip";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { ServiceList } from "@/features/services/service-list";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// The services list marks the ingress proxy service as TierPlan's (UI-SPEC S3, D-29).

const ENV = "e1";

type ServiceItem = components["schemas"]["ServiceItem"];

const svc = (extra: Partial<ServiceItem> = {}): ServiceItem => ({
  id: "id-web",
  name: "web",
  mode: "replicated",
  running: 2,
  desired: 2,
  updateState: "",
  image: "registry.example.com/acme/web:1.4",
  stack: "",
  tierplanStackId: "",
  agent: false,
  ingress: false,
  updatedAt: "2026-10-01T10:00:00Z",
  ports: [],
  updateMessage: "",
  hasPreviousSpec: false,
  createdAt: "2026-09-01T10:00:00Z",
  ...extra,
});

let client: QueryClient;

beforeEach(() => {
  const items = [
    svc(),
    svc({
      id: "id-proxy",
      name: "tierplan-ingress",
      mode: "global",
      running: 3,
      desired: 3,
      image: "tierplan/ingress:1",
      ingress: true,
    }),
  ];
  client = new QueryClient({ defaultOptions: { queries: { retry: false, retryDelay: 1 } } });
  setCsrfToken(null);
  vi.stubGlobal("EventSource", FakeEventSource);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(makeMe({ envs: { [ENV]: "operator" } }));
      if (path === `/api/env/${ENV}/services`) {
        return Response.json({
          items,
          total: items.length,
          unfilteredTotal: items.length,
          capped: false,
          statusCounts: { degraded: 0, updating: 0, problem: 0, loop: 0 },
          noManager: false,
          fetchedAt: "2026-10-14T09:00:00Z",
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
          <ServiceList envId={ENV} search={{}} onSearchChange={() => {}} />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

describe("the services list and the ingress proxy", () => {
  it("draws the TierPlan ingress badge, with its lock, on the proxy's row once", async () => {
    mount();
    await screen.findByText("web");
    const row = screen.getByRole("row", { name: /^Select tierplan-ingress/ });
    const badge = within(row).getByText("TierPlan ingress");
    expect(within(row).getAllByText("TierPlan ingress")).toHaveLength(1);
    // The lock is decoration: the words carry the meaning.
    const lock = badge.querySelector("svg");
    expect(lock?.getAttribute("aria-hidden")).toBe("true");
    // The badge never shrinks beside a long name.
    expect(badge.className).toContain("shrink-0");
    expect(screen.getAllByText("TierPlan ingress")).toHaveLength(1);
  });

  it("draws no badge on a plain row", async () => {
    mount();
    await screen.findByText("web");
    expect(
      within(screen.getByRole("row", { name: /^Select web/ })).queryByText("TierPlan ingress"),
    ).toBeNull();
  });
});
