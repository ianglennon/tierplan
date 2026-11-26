// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { NodesTab } from "@/features/ingress/nodes-tab";
import { vipFixture } from "@/features/ingress/vip-fixture";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

const ENV = "e1";
const CURRENT = "9f8e7d6c5b4a39281716";

type Json = Record<string, unknown>;

let nodes: () => Response;
let status: Json;
let envType: "swarm" | "standalone";
let vip: () => Response;
let vipRequests: number;
let client: QueryClient;
let me: ReturnType<typeof makeMe>;

function node(over: Json = {}): Json {
  return {
    id: "n1",
    name: "edge-1",
    swarmNodeId: "sw1",
    status: "active",
    version: "v0.4.2",
    schemas: [1],
    appliedBundleId: CURRENT,
    appliedAt: "2026-11-20T10:00:00Z",
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
    sync: "current",
    ...over,
  };
}

function serve(items: Json[], currentBundleId = CURRENT) {
  nodes = () => Response.json({ items, currentBundleId });
}

beforeEach(() => {
  setCsrfToken(null);
  serve([]);
  status = { enabled: true };
  envType = "standalone";
  vipRequests = 0;
  vip = () => Response.json(vipFixture());
  me = makeMe({ envs: { [ENV]: "viewer" } });
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(me);
      if (path === `/api/env/${ENV}/ingress/nodes`) return nodes();
      if (path === `/api/env/${ENV}/ingress/status`) return Response.json(status);
      if (path === "/api/environments") {
        return Response.json({ items: [{ id: ENV, name: "Prod", envType }] });
      }
      if (path === `/api/env/${ENV}/ingress/vip`) {
        vipRequests++;
        return vip();
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  client.clear();
});

function mount() {
  return render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <NodesTab envId={ENV} />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

async function table() {
  return screen.findByRole("table", { name: "Ingress nodes" });
}

const rowOf = (t: HTMLElement, name: string) =>
  within(t).getByText(name).closest("tr") as HTMLElement;

describe("the Ingress page's Nodes tab", () => {
  it("draws each node with its enrolment, supervisor, health, bundle and last seen in words", async () => {
    serve([
      node(),
      node({
        id: "n2",
        name: "edge-2",
        swarmNodeId: "sw2",
        connected: false,
        health: "unhealthy",
        appliedBundleId: "1a2b3c4d5e6f",
        sync: "pending",
        version: "v0.4.1",
      }),
      node({
        id: "n3",
        name: "edge-3",
        swarmNodeId: "",
        connected: false,
        health: "",
        appliedBundleId: "",
        appliedAt: undefined,
        lastSeenAt: undefined,
        version: "",
        sync: "pending",
      }),
      node({
        id: "n4",
        name: "edge-4",
        status: "revoked",
        connected: false,
        sync: "unknown",
        revokedAt: "2026-11-21T10:00:00Z",
      }),
    ]);
    mount();
    const t = await table();

    const one = rowOf(t, "edge-1");
    expect(within(one).getByText("Connected")).toBeTruthy();
    expect(within(one).getByText("v0.4.2")).toBeTruthy();
    expect(within(one).getByText("Healthy")).toBeTruthy();
    expect(within(one).getByText("Current")).toBeTruthy();
    expect(within(one).getByText("Applied 9f8e7d6c")).toBeTruthy();
    expect(within(one).queryByText(/^Current 9f8e/)).toBeNull();
    expect(within(one).getByText(/Enrolled/)).toBeTruthy();

    const two = rowOf(t, "edge-2");
    expect(within(two).getByText("Not connected")).toBeTruthy();
    expect(within(two).getByText("Unhealthy")).toBeTruthy();
    expect(within(two).getByText("Updating")).toBeTruthy();
    expect(within(two).getByText("Applied 1a2b3c4d")).toBeTruthy();
    expect(within(two).getByText("Current 9f8e7d6c")).toBeTruthy();

    const three = rowOf(t, "edge-3");
    expect(within(three).getByText("Never")).toBeTruthy();
    expect(within(three).getByText("Applied none")).toBeTruthy();
    expect(within(three).getByText("Not reported")).toBeTruthy();

    const four = rowOf(t, "edge-4");
    // The word beside the dot, and "Revoked <time>" under it.
    expect(within(four).getAllByText("Revoked")).toHaveLength(2);
    expect(four.querySelector("time[datetime='2026-11-21T10:00:00Z']")).not.toBeNull();
    expect(within(four).queryByRole("button", { name: "Revoke" })).toBeNull();
  });

  it("never says how many versions a node is behind", async () => {
    serve([node({ appliedBundleId: "1a2b3c4d5e6f", sync: "pending" })]);
    mount();
    await table();
    expect(screen.queryByText(/behind by/i)).toBeNull();
  });

  it("links a swarm node to its page and draws a node of a standalone host as text", async () => {
    serve([node(), node({ id: "n2", name: "solo", swarmNodeId: "" })]);
    mount();
    const t = await table();
    const link = within(rowOf(t, "edge-1")).getByRole("link", { name: "edge-1" });
    expect(link.getAttribute("href")).toBe(`/env/${ENV}/nodes/sw1`);
    expect(within(rowOf(t, "solo")).queryByRole("link")).toBeNull();
  });

  it("reads each node's sync state in a word", async () => {
    serve([
      node({ id: "a", name: "a-node", sync: "incompatible", incompatible: true }),
      node({ id: "b", name: "b-node", sync: "unknown", appliedBundleId: "" }),
    ]);
    mount();
    const t = await table();
    expect(within(rowOf(t, "a-node")).getByText("Can't apply")).toBeTruthy();
    expect(within(rowOf(t, "b-node")).getByText("Unknown")).toBeTruthy();
  });

  it("draws the empty state, the error with its retry, and the off state", async () => {
    serve([]);
    const first = mount();
    expect(await screen.findByText("No nodes enrolled yet")).toBeTruthy();
    first.unmount();
    client.clear();

    nodes = () => Response.json({ title: "boom" }, { status: 500 });
    mount();
    expect(await screen.findByText("Couldn't read the nodes.")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Retry reading nodes" })).toBeTruthy();
  });

  it("draws the off state when ingress is off and no node ever enrolled", async () => {
    status = { enabled: false };
    serve([]);
    mount();
    expect(await screen.findByText("Ingress is off")).toBeTruthy();
    await waitFor(() => expect(screen.queryByText("No nodes enrolled yet")).toBeNull());
  });
  const revokedNode = () =>
    node({
      id: "n9",
      name: "gone-1",
      status: "revoked",
      connected: false,
      sync: "unknown",
      revokedAt: "2026-11-21T10:00:00Z",
      revokeReason: "decommissioned",
    });

  it("shows a revoked node's reason and the token warning to an Admin only", async () => {
    me = makeMe({ envs: { [ENV]: "admin" } });
    serve([revokedNode()]);
    const admin = mount();
    const t = await table();
    const row = rowOf(t, "gone-1");
    expect(within(row).getByText("decommissioned")).toBeTruthy();
    expect(within(row).getByText("The join token it held may still be mounted.")).toBeTruthy();
    admin.unmount();
    client.clear();

    // The server blanks the reason for a reader, and the row draws no label for it.
    me = makeMe({ envs: { [ENV]: "viewer" } });
    serve([{ ...revokedNode(), revokeReason: "", heldHostnames: null, revokedBy: "" }]);
    mount();
    const viewerRow = rowOf(await table(), "gone-1");
    expect(within(viewerRow).queryByText("decommissioned")).toBeNull();
    expect(within(viewerRow).queryByText(/may still be mounted/)).toBeNull();
    expect(within(viewerRow).getAllByText("Revoked")).toHaveLength(2);
    expect(viewerRow.textContent).not.toMatch(/Reason|Revoked by/);
  });

  it("says an incompatible node can't apply this server's settings and shows a refused bundle", async () => {
    serve([
      node({
        incompatible: true,
        sync: "incompatible",
        rejectedBundleId: "0a1b2c3d4e5f6071",
        rejectedReason: "schema_unsupported",
      }),
    ]);
    mount();
    const row = rowOf(await table(), "edge-1");
    expect(within(row).getByText("Can't apply this server's settings")).toBeTruthy();
    expect(within(row).getByText("Can't apply")).toBeTruthy();
    expect(within(row).getByText("Refused 0a1b2c3d: schema_unsupported")).toBeTruthy();
  });

  it("draws a loading status while the nodes are read", async () => {
    nodes = () => new Response(null, { status: 500 });
    mount();
    expect(screen.getByRole("status").textContent).toBe("Loading nodes");
    await screen.findByText("Couldn't read the nodes.");
  });

  it("reads the nodes again when Retry is pressed", async () => {
    nodes = () => Response.json({ title: "boom" }, { status: 500 });
    mount();
    const retry = await screen.findByRole("button", { name: "Retry reading nodes" });
    serve([node()]);
    retry.click();
    expect(await table()).toBeTruthy();
  });

  describe("the virtual IP card (D-06)", () => {
    it("draws the card before the node table on a swarm with the virtual IP on", async () => {
      envType = "swarm";
      serve([node()]);
      mount();
      const t = await table();
      const heading = await screen.findByRole("heading", { name: "Virtual IP" });
      expect(heading.compareDocumentPosition(t) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
      expect(screen.getByText("Held by node-b")).toBeTruthy();
      expect(screen.queryByText("The virtual IP is off. Turn it on in Settings.")).toBeNull();
    });

    function candidate(over: Json) {
      return {
        nodeId: "sw1",
        hostname: "edge-1",
        vrrpState: "BACKUP",
        proxyReady: true,
        taskState: "running",
        lastFault: "",
        lastHeard: "2026-11-20T10:05:00Z",
        ...over,
      };
    }

    it("joins each candidate's VRRP state onto its node row, and draws a dash for a node that is not one", async () => {
      envType = "swarm";
      vip = () =>
        Response.json(
          vipFixture({
            candidates: [
              candidate({ nodeId: "sw1", vrrpState: "MASTER" }),
              candidate({ nodeId: "sw2", vrrpState: "FAULT", lastFault: "script_failed" }),
              candidate({ nodeId: "sw3", vrrpState: "STOP" }),
              candidate({ nodeId: "sw4", vrrpState: "" }),
            ] as never,
          }),
        );
      serve([
        node(),
        node({ id: "n2", name: "edge-2", swarmNodeId: "sw2" }),
        node({ id: "n3", name: "edge-3", swarmNodeId: "sw3" }),
        node({ id: "n4", name: "edge-4", swarmNodeId: "sw4" }),
        node({ id: "n5", name: "edge-5", swarmNodeId: "sw9" }),
        node({ id: "n6", name: "solo", swarmNodeId: "" }),
      ]);
      mount();
      const t = await table();
      await screen.findByRole("columnheader", { name: "Virtual IP" });
      expect(within(rowOf(t, "edge-1")).getByText("Holds the address")).toBeTruthy();
      const two = rowOf(t, "edge-2");
      expect(within(two).getByText("Fault")).toBeTruthy();
      expect(within(two).getByText("script_failed")).toBeTruthy();
      expect(within(rowOf(t, "edge-3")).getByText("Stopped")).toBeTruthy();
      expect(within(rowOf(t, "edge-4")).getByText("Not reporting")).toBeTruthy();
      expect(within(rowOf(t, "edge-5")).getByText("—")).toBeTruthy();
      expect(within(rowOf(t, "solo")).getByText("—")).toBeTruthy();
    });

    it("draws a standby candidate as Standby with no fault line", async () => {
      envType = "swarm";
      vip = () => Response.json(vipFixture({ candidates: [candidate({})] as never }));
      serve([node()]);
      mount();
      const row = rowOf(await table(), "edge-1");
      expect(await within(row).findByText("Standby")).toBeTruthy();
      expect(within(row).queryByText("script_failed")).toBeNull();
    });

    it("has no Virtual IP column on a swarm with the virtual IP off or on a standalone environment", async () => {
      envType = "standalone";
      serve([node()]);
      mount();
      await table();
      expect(screen.queryByRole("columnheader", { name: "Virtual IP" })).toBeNull();
    });

    it("never asks for the virtual IP on a standalone environment and draws none of it", async () => {
      envType = "standalone";
      serve([node()]);
      mount();
      await table();
      await waitFor(() => expect(screen.queryByText("Loading nodes")).toBeNull());
      expect(vipRequests).toBe(0);
      expect(screen.queryByRole("heading", { name: "Virtual IP" })).toBeNull();
      expect(screen.queryByText(/The virtual IP is off/)).toBeNull();
    });

    it("says the virtual IP is off on a swarm that has none, not an error", async () => {
      envType = "swarm";
      vip = () =>
        Response.json(
          { type: "vip_not_enabled", title: "Not Found", status: 404, detail: "No VIP." },
          { status: 404, headers: { "Content-Type": "application/problem+json" } },
        );
      serve([node()]);
      mount();
      await table();
      expect(
        await screen.findByText("The virtual IP is off. Turn it on in Settings."),
      ).toBeTruthy();
      expect(screen.queryByRole("heading", { name: "Virtual IP" })).toBeNull();
      expect(screen.queryByRole("columnheader", { name: "Virtual IP" })).toBeNull();
      expect(screen.queryByText("Couldn't read the nodes.")).toBeNull();
    });
  });
});
