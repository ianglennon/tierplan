// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { EnvironmentItem } from "@/api/environments";
import { EnvDegradedBanner } from "@/features/environments/degraded-banner";
import "@/i18n";
import { makeMe } from "@/test-me";
import { splitPermissions } from "@/test-permissions";

type Issue = {
  code: string;
  node?: string;
  subject?: string;
  percent?: number;
  since?: string;
};

function degraded(issues: Issue[] | null, extra: Partial<EnvironmentItem["status"]> = {}) {
  const item: EnvironmentItem = {
    id: "e1",
    name: "prod swarm",
    kind: "agent",
    enabled: true,
    isSelfHost: false,
    isLocalSocket: false,
    createdAt: "2026-10-01T10:00:00Z",
    engine: {},
    envType: "swarm",
    status: { state: "degraded", since: "2026-10-03T09:00:00Z", issues, ...extra },
    agentVersion: null,
    agents: null,
    engineVersion: null,
    failingServices: null,
    addressSpace: null,
    lastSeen: null,
    nodes: null,
    updateAvailable: false,
  };
  return item;
}

const MIXED: Issue[] = [
  { code: "cert_expired", node: "n1" },
  { code: "agent_offline", node: "n2" },
  { code: "clock_skew", node: "n3" },
];

let permissions: { global: string[]; environments: Record<string, string[]> };

// A signed-in user built by role, for a test about who holds a role on the environment.
let account: ReturnType<typeof makeMe> | undefined;

function me() {
  return account ?? makeMe({ instanceAdmin: true, permissions });
}

beforeEach(() => {
  setCsrfToken(null);
  account = undefined;
  permissions = { global: [], environments: { "*": ["environments.read"] } };
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(me());
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

async function mount(environment: EnvironmentItem) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const root = createRootRoute({
    component: () => <EnvDegradedBanner environment={environment} />,
  });
  const settings = createRoute({
    getParentRoute: () => root,
    path: "/settings/environments/$envId",
    component: () => null,
  });
  const addressSpace = createRoute({
    getParentRoute: () => root,
    path: "/env/$envId/address-space",
    component: () => null,
  });
  const router = createRouter({
    routeTree: root.addChildren([settings, addressSpace]),
    history: createMemoryHistory({ initialEntries: ["/"] }),
  });
  render(
    <QueryClientProvider client={client}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
  // The banner is a role=alert; a state with none renders nothing at all.
  await waitFor(() => expect(router.state.status).toBe("idle"));
}

describe("EnvDegradedBanner", () => {
  it("names the environment, shows the most severe issue and counts the rest", async () => {
    await mount(degraded(MIXED));
    const banner = await screen.findByRole("alert");
    expect(banner.textContent).toContain("prod swarm is degraded");
    // agent_offline outranks cert_expired and clock_skew, whatever order they come in.
    expect(screen.getByText("The agent on n2 isn't connected.")).toBeTruthy();
    expect(screen.getByText("and 2 more issues")).toBeTruthy();
    expect(screen.queryByText(/certificate/)).toBeNull();
  });

  it("says 'and 1 more issue' for two and nothing for one", async () => {
    await mount(degraded([MIXED[1] as Issue, MIXED[2] as Issue]));
    expect(await screen.findByText("and 1 more issue")).toBeTruthy();
    cleanup();
    await mount(degraded([MIXED[1] as Issue]));
    await screen.findByRole("alert");
    expect(screen.queryByText(/more issue/)).toBeNull();
  });

  it("puts a lost quorum and a missing manager above a node that is offline", async () => {
    await mount(
      degraded([
        { code: "agent_offline", node: "n2" },
        { code: "not_a_manager" },
        { code: "no_quorum" },
      ]),
    );
    expect(await screen.findByText("This swarm has lost quorum.")).toBeTruthy();
    expect(screen.queryByText("The agent on n2 isn't connected.")).toBeNull();
    expect(screen.getByText("and 2 more issues")).toBeTruthy();
    cleanup();
    await mount(degraded([{ code: "agent_offline", node: "n2" }, { code: "not_a_manager" }]));
    await screen.findByRole("alert");
    expect(screen.queryByText("The agent on n2 isn't connected.")).toBeNull();
    expect(screen.getByText("and 1 more issue")).toBeTruthy();
  });

  it("falls back to the status reason when the issue list is empty", async () => {
    await mount(degraded(null, { reason: "no_quorum" }));
    expect(await screen.findByText("This swarm has lost quorum.")).toBeTruthy();
    expect(screen.queryByText(/more issue/)).toBeNull();
  });

  it("links View nodes to the environment's settings for environments.configure on it", async () => {
    permissions = splitPermissions(["environments.read", "environments.configure"]);
    await mount(degraded(MIXED));
    const link = await screen.findByRole("link", { name: "View nodes" });
    expect(link.getAttribute("href")).toBe("/settings/environments/e1");
  });

  it("links View nodes for an env Admin of that environment, and not for a Viewer of it or an Admin of another", async () => {
    account = makeMe({ envs: { e1: "admin" } });
    await mount(degraded(MIXED));
    expect(await screen.findByRole("link", { name: "View nodes" })).toBeTruthy();
    cleanup();

    for (const other of [makeMe({ envs: { e1: "viewer" } }), makeMe({ envs: { e9: "admin" } })]) {
      account = other;
      await mount(degraded(MIXED));
      await screen.findByRole("alert");
      await waitFor(() => expect(vi.mocked(fetch)).toHaveBeenCalled());
      expect(screen.queryByRole("link", { name: "View nodes" })).toBeNull();
      cleanup();
    }
  });

  it("leaves View nodes out without environments.configure", async () => {
    await mount(degraded(MIXED));
    await screen.findByRole("alert");
    // The permission read has to settle before the absence means anything.
    await waitFor(() => expect(vi.mocked(fetch)).toHaveBeenCalled());
    expect(screen.queryByRole("link", { name: "View nodes" })).toBeNull();
  });

  it("words the pool cause and opens the address space page", async () => {
    permissions = splitPermissions(["environments.read", "networks.read"]);
    await mount(degraded([{ code: "address_space_full", subject: "10.0.0.0/8", percent: 93 }], {}));
    expect(await screen.findByText("The address pool 10.0.0.0/8 is 93% used.")).toBeTruthy();
    expect(
      screen.getByText(
        "Remove orphaned networks or use smaller subnets before new stacks fail to deploy.",
      ),
    ).toBeTruthy();
    const link = screen.getByRole("link", { name: "Open address space" });
    expect(link.getAttribute("href")).toBe("/env/e1/address-space");
  });

  it("words the overlay cause from the summary's kind, or from the subject without one", async () => {
    const issue = { code: "address_space_full", subject: "shop_default", percent: 96 };
    await mount(degraded([issue]));
    expect(await screen.findByText("The network shop_default is 96% full.")).toBeTruthy();
    cleanup();
    // The summary names the same network, so its kind decides.
    const space = {
      worstName: "ingress",
      worstKind: "ingress" as const,
      used: 29,
      capacity: 30,
      percent: 96,
      state: "almost_full" as const,
      unit: "addresses" as const,
      estimated: false,
      orphaned: 0,
    };
    await mount({
      ...degraded([{ ...issue, subject: "ingress" }]),
      addressSpace: space,
    });
    expect(await screen.findByText("The network ingress is 96% full.")).toBeTruthy();
  });

  it("falls back to the generic text without a subject or a percent", async () => {
    await mount(degraded([{ code: "address_space_full" }]));
    expect(await screen.findByText("An address pool or network is almost full.")).toBeTruthy();
    cleanup();
    await mount(degraded([{ code: "address_space_full", subject: "shop_default" }]));
    expect(await screen.findByText("An address pool or network is almost full.")).toBeTruthy();
  });

  it("leaves the action out without networks.read", async () => {
    await mount(degraded([{ code: "address_space_full", subject: "10.0.0.0/8", percent: 93 }]));
    await screen.findByRole("alert");
    await waitFor(() => expect(vi.mocked(fetch)).toHaveBeenCalled());
    expect(screen.queryByRole("link", { name: "Open address space" })).toBeNull();
  });

  it("ranks the cause last, below every connection issue", async () => {
    await mount(
      degraded([
        { code: "address_space_full", subject: "10.0.0.0/8", percent: 93 },
        MIXED[2] as Issue,
      ]),
    );
    expect(await screen.findByText("The clock on a node is out of step.")).toBeTruthy();
    expect(screen.queryByText(/address pool/)).toBeNull();
    expect(screen.getByText("and 1 more issue")).toBeTruthy();
    expect(screen.queryByRole("link", { name: "Open address space" })).toBeNull();
  });

  it("renders nothing for an environment that is online or has any other state", async () => {
    await mount({ ...degraded(MIXED), status: { state: "online" } });
    expect(screen.queryByRole("alert")).toBeNull();
    cleanup();
    await mount({ ...degraded(MIXED), status: { state: "offline", reason: "agent_offline" } });
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("shows a hostile node name and environment name as text, never as markup", async () => {
    const hostile = '<img src=x onerror="alert(1)">';
    const environment = {
      ...degraded([{ code: "agent_offline", node: hostile }]),
      name: "<b>pwn</b>",
    };
    await mount(environment);
    const banner = await screen.findByRole("alert");
    expect(banner.textContent).toContain(hostile);
    expect(banner.textContent).toContain("<b>pwn</b> is degraded");
    expect(banner.querySelector("img, b")).toBeNull();
  });
});
