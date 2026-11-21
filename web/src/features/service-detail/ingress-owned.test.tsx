// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { queryClient } from "@/api/query-client";
import { serviceRetry } from "@/api/services";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { routeTree } from "@/routeTree.gen";
import "@/i18n";
import { makeMe } from "@/test-me";
import {
  SERVICE_ENV,
  SERVICE_ID,
  type ServiceApiState,
  serviceDetail,
  serviceFetch,
} from "@/test-service";

// The ingress proxy's service page: the badge in the header (UI-SPEC S3, D-29).

const PAGE = `/env/${SERVICE_ENV}/services/${SERVICE_ID}`;

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
