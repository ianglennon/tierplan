// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { detailRetry } from "@/api/container-detail";
import type { NetworkDetail } from "@/api/networks";
import { queryClient } from "@/api/query-client";
import type { NetworkRow } from "@/api/resources";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { ConnectDialog } from "@/features/networks/connect-dialog";
import { DisconnectDialog } from "@/features/networks/disconnect-dialog";
import { NetworkList } from "@/features/networks/network-list";
import { routeTree } from "@/routeTree.gen";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// The ingress network is TierPlan's (UI-SPEC S3, D-29, D-46, DEP-08): badged, read-only with
// the reason, Inspect still open, and the proxy's own task container asks for its name.

const ENV = "e1";
const REASON = "Managed by TierPlan. Switch ingress off to remove it.";

let sent: { method: string; path: string; body?: Record<string, unknown> }[];
let client: QueryClient;
let items: NetworkRow[];
let detail: NetworkDetail;
let containers: unknown;

const net = (name: string, extra: Partial<NetworkRow> = {}): NetworkRow => ({
  id: `id-${name}`,
  name,
  driver: "overlay",
  scope: "swarm",
  node: "node-1",
  nodeId: "n1",
  subnets: [],
  created: "2026-10-01T10:00:00Z",
  builtIn: false,
  ingress: false,
  attachedContainers: 0,
  attachedServices: 0,
  owner: "",
  ownerKind: "",
  internal: false,
  attachable: false,
  ipv6: false,
  ...extra,
});

function detailOf(extra: Partial<NetworkDetail> = {}): NetworkDetail {
  return {
    id: "id-tierplan-ingress",
    name: "tierplan-ingress",
    driver: "overlay",
    scope: "swarm",
    node: "node-1",
    nodeId: "n1",
    created: "2026-10-01T10:00:00Z",
    builtIn: false,
    ingress: true,
    internal: false,
    attachable: false,
    ipv6: false,
    owner: "",
    ownerKind: "",
    labels: {},
    options: {},
    ipam: [],
    attachments: [],
    attachedContainers: 0,
    attachedServices: 0,
    aliasesTruncated: false,
    notReporting: [],
    ...extra,
  } as NetworkDetail;
}

function container(name: string, selfKind = "") {
  return {
    id: `${name}-id`,
    name,
    node: "node-1",
    nodeId: "n1",
    image: "traefik:3",
    state: "running",
    status: "Up",
    health: "none",
    ports: [],
    service: "",
    stack: "",
    project: "",
    isSelf: false,
    selfKind,
    readOnly: false,
    created: "2026-10-01T10:00:00Z",
  };
}

beforeEach(() => {
  sent = [];
  items = [net("tierplan-ingress", { ingress: true }), net("app-net")];
  detail = detailOf();
  containers = {
    items: [container("proxy.1.abc", "ingress"), container("web")],
    total: 2,
    truncated: false,
    nodes: ["node-1"],
    notReporting: [],
  };
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  queryClient.setDefaultOptions({ queries: { retry: false, refetchOnWindowFocus: false } });
  detailRetry.attempts = 0;
  setCsrfToken(null);
  FakeEventSource.reset();
  vi.stubGlobal("EventSource", FakeEventSource);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      sent.push({
        method: req.method,
        path: url.pathname,
        body: req.method === "POST" ? await req.clone().json() : undefined,
      });
      switch (url.pathname) {
        case "/api/instance":
          return Response.json({ instanceName: "Lab", needsSetup: false, rememberDays: 30 });
        case "/api/me":
          return Response.json(makeMe({ envs: { [ENV]: "admin" } }));
        case "/api/version":
          return Response.json({
            commit: "",
            display: "v0.1.0",
            sourceTreeUrl: "https://example.test/tree",
            sourceUrl: "https://example.test",
            version: "v0.1.0",
          });
        case "/api/environments":
          return Response.json({
            items: [
              {
                id: ENV,
                name: "Swarm",
                kind: "local",
                type: "swarm",
                envType: "swarm",
                enabled: true,
                isSelfHost: true,
                createdAt: "2026-10-01T10:00:00Z",
                status: { state: "online" },
              },
            ],
            total: 1,
          });
        case `/api/env/${ENV}/summary`:
          return Response.json({
            environment: {
              id: ENV,
              name: "Swarm",
              type: "swarm",
              status: { state: "online" },
            },
            engine: { name: "Docker Engine", version: "29.1.0", apiVersion: "1.52" },
            counts: {
              containers: 1,
              containersRunning: 1,
              containersStopped: 0,
              images: 0,
              volumes: 1,
              networks: 2,
            },
            isWorker: false,
            fetchedAt: "2026-10-01T10:00:00Z",
          });
        case `/api/env/${ENV}/networks`:
          return Response.json({
            items,
            total: items.length,
            truncated: false,
            nodes: ["node-1"],
            notReporting: [],
          });
        case `/api/env/${ENV}/networks/id-tierplan-ingress`:
          return Response.json(detail);
        case `/api/env/${ENV}/networks/id-app-net`:
          return Response.json(
            detailOf({ id: "id-app-net", name: "app-net", ingress: false, attachments: [] }),
          );
        case `/api/env/${ENV}/containers`:
          return Response.json(containers);
        default:
          return Response.json({}, { status: 404 });
      }
    }),
  );
});

afterEach(() => {
  toast.dismiss();
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  client.clear();
  queryClient.clear();
  detailRetry.attempts = 1;
  document.title = "";
});

const writes = () => sent.filter((s) => s.method !== "GET");

function mountList() {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <NetworkList envId={ENV} onNodeChange={() => {}} onSearchChange={() => {}} />
          <Toaster />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

function renderAt(path: string) {
  const router = createRouter({
    routeTree,
    history: createMemoryHistory({ initialEntries: [path] }),
  });
  render(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
}

describe("the networks list", () => {
  it("badges the ingress network and keeps Open enabled while Delete is aria-disabled with the reason", async () => {
    mountList();
    const row = (await screen.findByRole("link", { name: "tierplan-ingress" })).closest(
      "tr",
    ) as HTMLElement;
    expect(within(row).getByText("TierPlan ingress")).toBeTruthy();
    const del = within(row).getByRole("button", { name: "Delete network tierplan-ingress" });
    expect(del.getAttribute("aria-disabled")).toBe("true");
    expect(del.hasAttribute("disabled")).toBe(false);
    expect(within(row).getByRole("link", { name: /Open network tierplan-ingress/ })).toBeTruthy();

    // The reason opens on keyboard focus, not only on hover.
    fireEvent.focus(del);
    expect((await screen.findAllByText(REASON)).length).toBeGreaterThan(0);
    fireEvent.click(del);
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(writes()).toEqual([]);
  });

  it("leaves a plain network without the badge and with Delete open", async () => {
    mountList();
    const row = (await screen.findByRole("link", { name: "app-net" })).closest("tr") as HTMLElement;
    expect(within(row).queryByText("TierPlan ingress")).toBeNull();
    const del = within(row).getByRole("button", { name: "Delete network app-net" });
    expect(del.getAttribute("aria-disabled")).toBeNull();
  });
});

describe("the ingress network's page", () => {
  it("draws the badge and the neutral notice, disables Delete and Connect with the reason, and leaves Inspect", async () => {
    renderAt(`/env/${ENV}/networks/id-tierplan-ingress/attachments?node=n1`);
    const heading = await screen.findByRole("heading", { level: 2, name: "tierplan-ingress" });
    expect(heading).toBeTruthy();
    expect(screen.getByText("TierPlan ingress")).toBeTruthy();

    const notice = screen.getByText("Managed by TierPlan ingress").closest("[role]") as HTMLElement;
    // Plain status: an object TierPlan manages is expected, not a fault.
    expect(notice.getAttribute("role")).toBe("status");
    expect(
      within(notice).getByText(
        "TierPlan creates and removes this itself. To remove it, switch ingress off.",
      ),
    ).toBeTruthy();

    const del = screen.getByRole("button", { name: "Delete network tierplan-ingress" });
    expect(del.getAttribute("aria-disabled")).toBe("true");
    const connect = await screen.findByRole("button", { name: "Connect container" });
    expect(connect.getAttribute("aria-disabled")).toBe("true");
    fireEvent.focus(connect);
    expect((await screen.findAllByText(REASON)).length).toBeGreaterThan(0);

    const inspect = screen.getByRole("tab", { name: "Inspect" });
    expect(inspect.getAttribute("aria-disabled")).toBeNull();
    expect(writes()).toEqual([]);
  });

  it("disables Disconnect on every attached container", async () => {
    detail = detailOf({
      attachments: [
        {
          kind: "container",
          id: "proxy-id",
          name: "proxy.1.abc",
          node: "node-1",
          nodeId: "n1",
          state: "running",
          ipv4: "10.0.0.2",
          ipv6: "",
          mac: "",
          aliases: [],
          tasks: 0,
        },
      ],
      attachedContainers: 1,
    });
    renderAt(`/env/${ENV}/networks/id-tierplan-ingress/attachments?node=n1`);
    const disconnect = await screen.findByRole("button", {
      name: "Disconnect proxy.1.abc from tierplan-ingress",
    });
    expect(disconnect.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(disconnect);
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(writes()).toEqual([]);
  });

  it("shows no badge, no notice and open actions on a plain network", async () => {
    renderAt(`/env/${ENV}/networks/id-app-net/attachments?node=n1`);
    await screen.findByRole("heading", { level: 2, name: "app-net" });
    expect(screen.queryByText("TierPlan ingress")).toBeNull();
    expect(screen.queryByText("Managed by TierPlan ingress")).toBeNull();
    expect(
      (await screen.findByRole("button", { name: "Connect container" })).getAttribute(
        "aria-disabled",
      ),
    ).toBeNull();
  });
});

describe("the proxy's own task container (D-46)", () => {
  it("asks for the name before it connects, as it does for the agent", async () => {
    render(
      <QueryClientProvider client={client}>
        <TestRouter>
          <TooltipProvider>
            <ConnectDialog
              envId={ENV}
              open
              onOpenChange={() => {}}
              network={{
                id: "id-app-net",
                name: "app-net",
                scope: "swarm",
                nodeId: "n1",
                attachable: true,
              }}
            />
          </TooltipProvider>
        </TestRouter>
      </QueryClientProvider>,
    );
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(await within(dialog).findByRole("combobox", { name: "Container" }));
    fireEvent.click(await screen.findByRole("option", { name: /web/ }));
    expect(within(dialog).queryByRole("textbox", { name: /to confirm/ })).toBeNull();
    fireEvent.click(within(dialog).getByRole("combobox", { name: "Container" }));
    fireEvent.click(await screen.findByRole("option", { name: /proxy\.1\.abc/ }));
    const input = await within(dialog).findByRole("textbox", {
      name: "Type proxy.1.abc to confirm",
    });
    expect(
      within(dialog).getByRole("button", { name: "Connect container" }).hasAttribute("disabled"),
    ).toBe(true);
    fireEvent.change(input, { target: { value: "proxy.1.abc" } });
    await waitFor(() =>
      expect(
        within(dialog).getByRole("button", { name: "Connect container" }).hasAttribute("disabled"),
      ).toBe(false),
    );
  });

  it("asks for the name before it disconnects", async () => {
    render(
      <QueryClientProvider client={client}>
        <TooltipProvider>
          <DisconnectDialog
            envId={ENV}
            target={{
              networkId: "id-app-net",
              networkName: "app-net",
              containerId: "proxy-id",
              containerName: "proxy.1.abc",
              nodeId: "n1",
              selfKind: "ingress",
            }}
            onClose={() => {}}
          />
        </TooltipProvider>
      </QueryClientProvider>,
    );
    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByText(/one of TierPlan's own containers/)).toBeTruthy();
    expect(
      within(dialog).getByRole("textbox", { name: "Type proxy.1.abc to confirm" }),
    ).toBeTruthy();
    fireEvent.click(within(dialog).getByRole("button", { name: "Disconnect container" }));
    expect(writes()).toEqual([]);
  });
});
