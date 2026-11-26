// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { NodesTab } from "@/features/ingress/nodes-tab";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

const ENV = "e1";
const CURRENT = "9f8e7d6c5b4a39281716";

type Json = Record<string, unknown>;

let nodes: () => Response;
let status: Json;
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
  me = makeMe({ envs: { [ENV]: "viewer" } });
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(me);
      if (path === `/api/env/${ENV}/ingress/nodes`) return nodes();
      if (path === `/api/env/${ENV}/ingress/status`) return Response.json(status);
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
});
