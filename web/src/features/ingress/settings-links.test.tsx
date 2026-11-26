// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactElement } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { CertificatesTab } from "@/features/ingress/certificates-tab";
import { NodesTab } from "@/features/ingress/nodes-tab";
import { RoutesTab } from "@/features/ingress/routes-tab";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// Every link on the Ingress page that points at the Settings tab (D-03, UI-SPEC S4, S8): the "Turn on ingress" call
// to action of the off state, the Nodes tab's virtual IP line, and the post-revoke notice. They only navigate: the
// switch itself stays behind the Settings tab's review dialog.

const ENV = "e1";
const SETTINGS = `/env/${ENV}/ingress/settings`;

type Json = Record<string, unknown>;

let ingressOn: boolean;
let nodes: Json[];
let me: ReturnType<typeof makeMe>;
let client: QueryClient;

beforeEach(() => {
  setCsrfToken(null);
  ingressOn = false;
  nodes = [];
  me = makeMe({ envs: { [ENV]: "admin" } });
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(me);
      if (path === `/api/env/${ENV}/ingress/status`) {
        return Response.json({ enabled: ingressOn });
      }
      if (path === `/api/env/${ENV}/ingress/routes`) {
        return Response.json({ enabled: ingressOn, routes: [] });
      }
      if (path === `/api/env/${ENV}/ingress/certificates`) return Response.json({ items: [] });
      if (path === `/api/env/${ENV}/ingress/certificates/budget`) {
        return Response.json({ items: [] });
      }
      if (path === `/api/env/${ENV}/ingress/nodes`) {
        return Response.json({ items: nodes, currentBundleId: "" });
      }
      if (path === `/api/env/${ENV}/ingress/nodes/n1/revoke`) {
        return Response.json({
          heldHostnames: [],
          rotateTokenRecommended: true,
          revokedAt: "2026-11-26T10:00:00Z",
        });
      }
      if (path === "/api/environments") {
        return Response.json({
          items: [{ id: ENV, name: "Prod", envType: "swarm", enabled: true }],
          total: 1,
        });
      }
      if (path === `/api/env/${ENV}/ingress/vip`) {
        return Response.json(
          { type: "vip_not_enabled", title: "Not Found", status: 404, detail: "No VIP." },
          { status: 404, headers: { "Content-Type": "application/problem+json" } },
        );
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

function mount(tab: ReactElement) {
  return render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>{tab}</TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

const TABS: [string, () => ReactElement][] = [
  ["Routes", () => <RoutesTab envId={ENV} />],
  ["Certificates", () => <CertificatesTab envId={ENV} />],
  ["Nodes", () => <NodesTab envId={ENV} />],
];

describe("the off state's call to action", () => {
  for (const [name, tab] of TABS) {
    it(`links an Admin to the Settings tab from the ${name} tab`, async () => {
      mount(tab());
      const link = await screen.findByRole("link", { name: "Turn on ingress" });
      expect(link.getAttribute("href")).toBe(SETTINGS);
      expect(screen.queryByText(/Ask an Admin/)).toBeNull();
    });

    it(`tells a Viewer to ask an Admin on the ${name} tab, with no button`, async () => {
      me = makeMe({ envs: { [ENV]: "viewer" } });
      mount(tab());
      expect(await screen.findByText("Ingress is off")).toBeTruthy();
      expect(screen.getByText(/Ask an Admin to turn it on\./)).toBeTruthy();
      expect(screen.queryByRole("link", { name: "Turn on ingress" })).toBeNull();
      expect(screen.queryByRole("button", { name: "Turn on ingress" })).toBeNull();
    });
  }
});

describe("the Nodes tab's virtual IP line", () => {
  const line = "The virtual IP is off. Turn it on in Settings.";
  const node = () => ({
    id: "n1",
    name: "edge-1",
    swarmNodeId: "sw1",
    status: "active",
    version: "v0.4.2",
    schemas: [1],
    appliedBundleId: "",
    rejectedBundleId: "",
    rejectedReason: "",
    health: "ok",
    lastSeenAt: "2026-11-20T10:05:00Z",
    createdAt: "2026-11-01T10:00:00Z",
    revokedBy: "",
    revokeReason: "",
    reenrolBlocked: false,
    heldHostnames: [],
    incompatible: false,
    connected: true,
    sync: "unknown",
  });

  it("links Settings for an Admin", async () => {
    ingressOn = true;
    nodes = [node()];
    mount(<NodesTab envId={ENV} />);
    const link = await screen.findByRole("link", { name: "Settings" });
    expect(link.getAttribute("href")).toBe(SETTINGS);
    expect(link.parentElement?.textContent).toBe(line);
  });

  it("is plain text for a Viewer", async () => {
    me = makeMe({ envs: { [ENV]: "viewer" } });
    ingressOn = true;
    nodes = [node()];
    mount(<NodesTab envId={ENV} />);
    expect(await screen.findByText(line)).toBeTruthy();
    expect(screen.queryByRole("link", { name: "Settings" })).toBeNull();
  });
});

describe("after a revoke", () => {
  it("offers Go to the join token, linking to the join token card on the Settings tab", async () => {
    ingressOn = true;
    nodes = [
      {
        id: "n1",
        name: "edge-1",
        swarmNodeId: "sw1",
        status: "active",
        version: "v0.4.2",
        schemas: [1],
        appliedBundleId: "",
        rejectedBundleId: "",
        rejectedReason: "",
        health: "ok",
        lastSeenAt: "2026-11-20T10:05:00Z",
        createdAt: "2026-11-01T10:00:00Z",
        revokedBy: "",
        revokeReason: "",
        reenrolBlocked: false,
        heldHostnames: [],
        incompatible: false,
        connected: true,
        sync: "unknown",
      },
    ];
    mount(<NodesTab envId={ENV} />);
    fireEvent.click(await screen.findByRole("button", { name: "Revoke edge-1" }));
    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).queryByRole("link", { name: "Go to the join token" })).toBeNull();
    fireEvent.click(within(dialog).getByRole("button", { name: "Revoke node" }));
    await screen.findByText("Revoked edge-1");
    const link = within(screen.getByRole("alertdialog")).getByRole("link", {
      name: "Go to the join token",
    });
    await waitFor(() => expect(link.getAttribute("href")).toBe(`${SETTINGS}#join-token`));
  });
});
