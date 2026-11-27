// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { HTTPSSelfRoute } from "@/api/https";
import { queryClient } from "@/api/query-client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { HttpsPage } from "@/features/https/https-page";
import { SelfRouteLine } from "@/features/https/self-route-line";
import "@/i18n";
import { httpsStatus } from "@/test-https";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

const HOST = "tp.apps.example.org";
const SENTENCE = `TierPlan is also served at ${HOST} through ingress.`;

let status: () => Response | Promise<Response>;
let me: () => Response;

const instanceAdmin = () => Response.json(makeMe({ instanceAdmin: true }));

function selfRouteOf(over: Partial<HTTPSSelfRoute> = {}): HTTPSSelfRoute {
  return { envId: "env-1", host: HOST, state: "serving", ...over };
}

beforeEach(() => {
  status = () => Response.json(httpsStatus());
  me = instanceAdmin;
  FakeEventSource.reset();
  vi.stubGlobal("EventSource", FakeEventSource);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return me();
      if (path === "/api/https/status") return status();
      if (path === "/api/https/acme/directory") return Response.json({ profiles: [] });
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  queryClient.clear();
});

function renderUI(ui: React.ReactNode) {
  render(
    <QueryClientProvider client={queryClient}>
      <TooltipProvider>
        <TestRouter>{ui}</TestRouter>
      </TooltipProvider>
    </QueryClientProvider>,
  );
}

const group = () => screen.queryByRole("group", { name: "TierPlan's own route" });

// The line drawn on its own, once who is signed in has arrived (the link waits on it).
async function lineReady(selfRoute: HTTPSSelfRoute) {
  renderUI(<SelfRouteLine selfRoute={selfRoute} />);
  await waitFor(() => expect(queryClient.getQueryData(["me"])).toBeTruthy());
  return within(group() as HTMLElement);
}

describe("TierPlan's own route on Settings > HTTPS (D-16, UI-SPEC S2)", () => {
  it("draws the line when the route is on", async () => {
    status = () => Response.json(httpsStatus({ selfRoute: selfRouteOf() }));
    renderUI(<HttpsPage />);
    const line = await screen.findByRole("group", { name: "TierPlan's own route" });
    await waitFor(() => expect(within(line).queryByRole("link")).toBeTruthy());

    expect(line.textContent).toContain(SENTENCE);
    const host = within(line).getByText(HOST);
    expect(host.className).toContain("font-mono");
    expect(host.className).toContain("break-all");
    expect(within(line).getByText("Serving")).toBeTruthy();
    expect(line.className).toContain("bg-muted/50");

    const link = within(line).getByRole("link", { name: "See the route settings" });
    expect(link.getAttribute("href")).toBe("/env/env-1/ingress/settings#tierplan-own-route");

    // Under the heading block, above the certificate card.
    const heading = screen.getByRole("heading", { name: "HTTPS" });
    const region = await screen.findByRole("region", { name: "Certificate in use" });
    expect(heading.compareDocumentPosition(line) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(line.compareDocumentPosition(region) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("draws nothing when off, loading or on error", async () => {
    // Off: the status carries no selfRoute.
    renderUI(<HttpsPage />);
    await screen.findByRole("region", { name: "Certificate in use" });
    expect(group()).toBeNull();
    expect(screen.queryByText(/through ingress/)).toBeNull();
    cleanup();
    queryClient.clear();

    // Loading: the status never arrives.
    status = () => new Promise<Response>(() => {});
    renderUI(<HttpsPage />);
    await waitFor(() => expect(queryClient.getQueryData(["me"])).toBeTruthy());
    expect(group()).toBeNull();
    cleanup();
    queryClient.clear();

    // A read error draws nothing either.
    status = () =>
      Response.json(
        { type: "about:blank", title: "x", status: 500, detail: "d" },
        { status: 500, headers: { "Content-Type": "application/problem+json" } },
      );
    renderUI(<HttpsPage />);
    await waitFor(() =>
      expect(queryClient.getQueryState(["https", "status"])?.status).toBe("error"),
    );
    expect(group()).toBeNull();
    expect(screen.queryByText(/through ingress/)).toBeNull();
  });

  it("drops the link without ingress.read", async () => {
    me = () => Response.json(makeMe({ envs: { "env-2": "admin" } }));
    const line = await lineReady(selfRouteOf());
    expect((group() as HTMLElement).textContent).toContain(SENTENCE);
    expect(line.getByText("Serving")).toBeTruthy();
    expect(line.queryByRole("link")).toBeNull();
    expect(line.queryByText("See the route settings")).toBeNull();
    expect((group() as HTMLElement).querySelector("[data-slot=tooltip-trigger]")).toBeNull();
  });

  it("keeps the link for a viewer of that environment", async () => {
    me = () => Response.json(makeMe({ envs: { "env-1": "viewer" } }));
    const line = await lineReady(selfRouteOf());
    expect(line.getByRole("link", { name: "See the route settings" })).toBeTruthy();
  });

  it("draws Unknown for an unreadable state", async () => {
    const line = await lineReady(selfRouteOf({ state: "unknown" }));
    expect(line.getByText("Unknown")).toBeTruthy();
    expect(screen.queryByRole("alert")).toBeNull();
    expect(document.querySelector("[data-slot=alert]")).toBeNull();
  });

  it("uses the card's words for the other states", async () => {
    const words: [HTTPSSelfRoute["state"], string][] = [
      ["pending", "Rolling out"],
      ["waiting_certificate", "Waiting for certificate"],
      ["self_not_attached", "Not attached"],
    ];
    for (const [state, word] of words) {
      const line = await lineReady(selfRouteOf({ state }));
      expect(line.getByText(word)).toBeTruthy();
      cleanup();
      queryClient.clear();
    }
  });
});
