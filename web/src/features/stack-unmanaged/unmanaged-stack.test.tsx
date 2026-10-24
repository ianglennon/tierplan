// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { UnmanagedOverview } from "@/features/stack-unmanaged/unmanaged-overview";
import { StackDetailLayout } from "@/features/stacks/stack-detail-layout";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// The read-only page of a stack Bayplan did not make (plan 08-09, STK-12, D-09, D-10, D-11, D-19).

const ENV = "e1";

type Service = {
  id: string;
  name: string;
  short: string;
  mode: string;
  state: string;
  updateState: string;
  running: number;
  desired: number;
  image: string;
  pinned: string;
  pinState: string;
  ports: string[];
  containers: { id: string; name: string; state: string; health: string }[];
};

const service = (short: string, over: Partial<Service> = {}): Service => ({
  id: `svc-${short}`,
  name: `legacy_${short}`,
  short,
  mode: "replicated",
  state: "running",
  updateState: "",
  running: 1,
  desired: 1,
  image: "nginx:1.27",
  pinned: "nginx:1.27@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
  pinState: "pinned",
  ports: [],
  containers: [],
  ...over,
});

let body: Record<string, unknown>;
let notFound: { managedStackId?: string } | null;
let client: QueryClient;

beforeEach(() => {
  body = {
    name: "legacy",
    target: "swarm",
    state: "degraded",
    servicesRunning: 1,
    servicesTotal: 2,
    services: [
      service("app", { ports: ["8080->80/tcp"] }),
      service("db", { running: 0, state: "not_running" }),
    ],
    foundBy: { label: "com.docker.stack.namespace", value: "legacy" },
    orphan: false,
    portainer: false,
    owner: "",
    adoptable: true,
    reason: "",
  };
  notFound = null;
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(makeMe({ envs: { [ENV]: "viewer" } }));
      if (path === "/api/environments") {
        return Response.json({ items: [{ id: ENV, name: "Prod", envType: "swarm" }] });
      }
      if (path === `/api/env/${ENV}/unmanaged-stacks/legacy`) {
        if (notFound) {
          return Response.json(
            {
              type: "unmanaged_stack_not_found",
              title: "Not Found",
              status: 404,
              detail: "gone",
              errors: notFound.managedStackId
                ? [{ location: "managedStackId", message: notFound.managedStackId }]
                : undefined,
            },
            { status: 404 },
          );
        }
        return Response.json(body);
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
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <StackDetailLayout variant="unmanaged" envId={ENV} name="legacy">
            <UnmanagedOverview envId={ENV} name="legacy" />
          </StackDetailLayout>
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

describe("the Unmanaged stack page: header and notes", () => {
  it("shows the name, its state, Swarm and Unmanaged, and the line saying Bayplan does not manage it", async () => {
    mount();
    expect(await screen.findByRole("heading", { name: "legacy", level: 2 })).toBeTruthy();
    expect(await screen.findByText("Degraded")).toBeTruthy();
    // The badge in the header and the Target row of the Details.
    expect(await screen.findAllByText("Swarm")).toHaveLength(2);
    // The badge in the header and the Source row of the Details.
    expect(await screen.findAllByText("Unmanaged")).toHaveLength(2);
    expect(await screen.findByText("Prod · Not managed by Bayplan")).toBeTruthy();
    const crumb = screen.getByRole("navigation", { name: "Breadcrumb" });
    expect(within(crumb).getByRole("link", { name: "Stacks" }).getAttribute("href")).toBe(
      "/env/e1/stacks",
    );
  });

  it("always shows the note that Bayplan doesn't manage the stack, as a status", async () => {
    mount();
    const note = (await screen.findByText("Bayplan doesn't manage this stack")).closest(
      "[data-slot=alert]",
    );
    expect(note?.getAttribute("role")).toBe("status");
    expect(
      screen.getByText(
        "You can look at it here. Adopt it to deploy, update and roll it back with Bayplan. Adopting changes nothing until you deploy.",
      ),
    ).toBeTruthy();
  });

  it("offers no Editor or History tab and no Deploy, Stop, Start, Remove or Adopt action", async () => {
    mount();
    await screen.findByRole("heading", { name: "legacy", level: 2 });
    expect(screen.getAllByRole("tab").map((tab) => tab.textContent)).toEqual(["Overview"]);
    for (const name of [/deploy/i, /stop/i, /start/i, /remove/i, /edit/i, /adopt/i]) {
      expect(screen.queryByRole("button", { name })).toBeNull();
      expect(screen.queryByRole("link", { name })).toBeNull();
    }
  });

  it("adds the orphan note and the Portainer advice only when they apply", async () => {
    mount();
    await screen.findByText("Bayplan doesn't manage this stack");
    expect(screen.queryByText("Created by Bayplan (another instance or a lost record)")).toBeNull();
    expect(screen.queryByText("This stack looks like it was created by Portainer")).toBeNull();
    cleanup();
    body = { ...body, orphan: true, portainer: true };
    mount();
    expect(
      await screen.findByText("Created by Bayplan (another instance or a lost record)"),
    ).toBeTruthy();
    const advice = screen
      .getByText("This stack looks like it was created by Portainer")
      .closest("[data-slot=alert]");
    // Advice is never an alert: nothing is wrong on the page.
    expect(advice?.getAttribute("role")).toBe("status");
    expect(
      within(advice as HTMLElement).getByText(
        "Remove it from Portainer without deleting it, or the two tools will fight over it. Bayplan doesn't contact Portainer.",
      ),
    ).toBeTruthy();
  });
});

describe("the Unmanaged stack page: Overview", () => {
  it("lists the services found by label with their state, replicas, image and ports, each swarm service linked to its page", async () => {
    mount();
    const link = await screen.findByRole("link", { name: "app" });
    expect(link.getAttribute("href")).toBe("/env/e1/services/svc-app");
    const row = link.closest("tr") as HTMLElement;
    expect(within(row).getByText("1/1")).toBeTruthy();
    expect(within(row).getByText("nginx:1.27")).toBeTruthy();
    expect(within(row).getByText("8080->80/tcp")).toBeTruthy();
    expect(
      within(screen.getByRole("link", { name: "db" }).closest("tr") as HTMLElement).getByText(
        "0/1",
      ),
    ).toBeTruthy();
  });

  it("shows the Details: environment, target, source Unmanaged, the label it was found by and the service count", async () => {
    mount();
    const details = (await screen.findByRole("heading", { name: "Details" })).closest(
      "[data-slot=card]",
    ) as HTMLElement;
    expect(within(details).getByRole("link", { name: "Prod" }).getAttribute("href")).toBe(
      "/env/e1/stacks",
    );
    expect(within(details).getByText("Swarm")).toBeTruthy();
    expect(within(details).getByText("Unmanaged")).toBeTruthy();
    expect(within(details).getByText("Label com.docker.stack.namespace = legacy")).toBeTruthy();
    expect(within(details).getByText("2 services")).toBeTruthy();
  });
});
