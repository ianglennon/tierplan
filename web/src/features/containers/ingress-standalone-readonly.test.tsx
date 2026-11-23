// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { type ContainerDetail, detailRetry } from "@/api/container-detail";
import { queryClient } from "@/api/query-client";
import type { ContainerRow, NetworkRow } from "@/api/resources";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { ContainerRowActions } from "@/features/containers/row-actions";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { ConnectDialog } from "@/features/networks/connect-dialog";
import { NetworkList } from "@/features/networks/network-list";
import { VolumeList } from "@/features/volumes/volume-list";
import { routeTree } from "@/routeTree.gen";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// The standalone ingress proxy is TierPlan's and read-only wherever the web offers a container
// write (D-09): the list row, its page, the shell, the network dialogs, and the bridge network and
// cache volume it owns. The server refuses regardless (25-08); these pin that the browser does not
// offer what will be refused.

const ENV = "e1";
const REASON = "TierPlan's ingress proxy for this host. Switch ingress off to remove it.";
const OWNED_REASON = "Managed by TierPlan. Switch ingress off to remove it.";

const PAGE = `/env/${ENV}/containers/proxy-id?node=n1`;
const BRIDGE = "tierplan-ingress";
const CACHE = "tierplan-ingress-cache";

interface Sent {
  method: string;
  path: string;
  body?: { targets?: { nodeId: string; name: string }[] };
}

let sent: Sent[];
let client: QueryClient;
let detailBody: ContainerDetail;
let containerItems: unknown[];
let networkItems: unknown[];
let volumeItems: unknown[];
let actionAnswer: () => Response;
const defaults = queryClient.getDefaultOptions();

function detailOf(extra: Partial<ContainerDetail> = {}): ContainerDetail {
  return {
    nodeId: "n1",
    node: "node-1",
    readOnly: false,
    ingressOwned: true,
    id: "proxy-id",
    name: "tierplan-ingress-proxy-1",
    image: "traefik:3",
    imageId: "sha256:abc",
    created: "2026-10-01T10:00:00Z",
    state: "running",
    health: "none",
    startedAt: "2026-10-01T10:00:05Z",
    exitCode: 0,
    oomKilled: false,
    restartCount: 0,
    command: [],
    entrypoint: [],
    commandMasked: false,
    restartPolicy: "no",
    restartMaxRetries: 0,
    ports: [],
    mounts: [],
    networks: [
      {
        networkId: "net-ingress",
        name: BRIDGE,
        ipv4: "172.20.0.2",
        ipv6: "",
        mac: "",
        aliases: [],
      },
    ],
    labels: {},
    env: [],
    logConfig: { type: "json-file", options: [] },
    masked: false,
    isSelf: false,
    selfKind: "ingress",
    project: "tierplan-ingress",
    stack: "",
    tierplanStackId: "",
    service: "proxy",
    muted: false,
    ...extra,
  } as ContainerDetail;
}

const net = (name: string, extra: Partial<NetworkRow> = {}): NetworkRow => ({
  id: `id-${name}`,
  name,
  driver: "bridge",
  scope: "local",
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

const volume = (name: string, extra: Record<string, unknown> = {}) => ({
  name,
  driver: "local",
  node: "node-1",
  nodeId: "node-1-id",
  created: "2026-10-01T10:00:00Z",
  usedByContainers: 0,
  usedByServices: 0,
  anonymous: false,
  ingress: false,
  project: "",
  stack: "",
  ...extra,
});

const listOf = (items: unknown[]) => ({
  items,
  total: items.length,
  truncated: false,
  nodes: ["node-1"],
  notReporting: [],
});

function row(extra: Partial<ContainerRow> = {}): ContainerRow {
  return {
    node: "node-1",
    nodeId: "n1",
    id: "proxy-id",
    name: "tierplan-ingress-proxy-1",
    state: "running",
    status: "Up 2 hours",
    image: "traefik:3",
    created: "2026-10-01T10:00:00Z",
    ports: [],
    isSelf: false,
    ingressOwned: true,
    project: "tierplan-ingress",
    stack: "",
    tierplanStackId: "",
    service: "proxy",
    health: "none",
    selfKind: "ingress",
    readOnly: false,
    ...extra,
  };
}

beforeEach(() => {
  sent = [];
  detailBody = detailOf();
  containerItems = [row(), row({ id: "web-id", name: "web", ingressOwned: false, selfKind: "" })];
  networkItems = [net(BRIDGE, { ingress: true }), net("app-net")];
  volumeItems = [volume(CACHE, { ingress: true, usedByContainers: 1 }), volume("a"), volume("b")];
  actionAnswer = () =>
    Response.json({ results: [{ nodeId: "n1", id: "proxy-id", name: "x", result: "done" }] });
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  queryClient.setDefaultOptions({ queries: { retry: false, refetchOnWindowFocus: false } });
  detailRetry.attempts = 0;
  setCsrfToken(null);
  FakeEventSource.reset();
  vi.stubGlobal("EventSource", FakeEventSource);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      const body =
        req.method === "POST"
          ? await req
              .clone()
              .json()
              .catch(() => undefined)
          : undefined;
      sent.push({ method: req.method, path, body });
      switch (path) {
        case "/api/instance":
          return Response.json({ instanceName: "Lab", needsSetup: false, rememberDays: 30 });
        case "/api/me":
          return Response.json(
            makeMe({ envs: { [ENV]: "admin" }, extras: { [ENV]: ["containers.exec"] } }),
          );
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
                name: "Lab host",
                kind: "local",
                type: "standalone",
                envType: "standalone",
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
              name: "Lab host",
              type: "standalone",
              status: { state: "online" },
            },
            engine: { name: "Docker Engine", version: "29.1.0", apiVersion: "1.52" },
            counts: {
              containers: 2,
              containersRunning: 2,
              containersStopped: 0,
              images: 0,
              volumes: 3,
              networks: 2,
            },
            isWorker: false,
            fetchedAt: "2026-10-01T10:00:00Z",
          });
        case `/api/env/${ENV}/containers/proxy-id`:
          return Response.json(detailBody);
        case `/api/env/${ENV}/containers`:
          return Response.json(listOf(containerItems));
        case `/api/env/${ENV}/containers/actions`:
        case `/api/env/${ENV}/containers/remove`:
          return actionAnswer();
        case `/api/env/${ENV}/networks`:
          return Response.json(listOf(networkItems));
        case `/api/env/${ENV}/networks/id-app-net`:
          return Response.json({
            ...net("app-net"),
            labels: {},
            options: {},
            ipam: [],
            attachments: [],
            aliasesTruncated: false,
            notReporting: [],
          });
        case `/api/env/${ENV}/volumes/delete`:
          return Response.json({
            results: (body?.targets ?? []).map((t: { nodeId: string; name: string }) => ({
              ...t,
              result: "done",
            })),
          });
        case `/api/env/${ENV}/volumes`:
          return Response.json({
            ...listOf(volumeItems),
            unfilteredTotal: volumeItems.length,
            usageCounts: { all: volumeItems.length, used: 1, unused: volumeItems.length - 1 },
          });
      }
      return Response.json({}, { status: 404 });
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
  queryClient.setDefaultOptions(defaults);
  detailRetry.attempts = 1;
  document.title = "";
});

// The page records the environment as recently opened with a PUT of its own; that is not an
// action of the control under test.
const writes = () =>
  sent.filter((s) => s.method !== "GET" && !s.path.startsWith("/api/me/recent-environments/"));

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

function mountRow(r: ContainerRow) {
  return render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <ContainerRowActions envId={ENV} row={r} />
          <Toaster />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

async function openMenu(name: string) {
  await waitFor(() => expect(sent.some((s) => s.path === "/api/me")).toBe(true));
  await act(async () => {});
  const trigger = screen.getByRole("button", { name: `Actions for ${name}` });
  await act(async () => {
    fireEvent.keyDown(trigger, { key: "Enter" });
  });
}

describe("TestStandaloneProxyRowIsReadOnly", () => {
  it("offers every action that fits a running proxy disabled with the reason, and Open", async () => {
    mountRow(row({ state: "running" }));
    await openMenu("tierplan-ingress-proxy-1");
    const open = await screen.findByRole("menuitem", { name: "Open" });
    expect(open.getAttribute("aria-disabled")).toBeNull();
    expect(open.getAttribute("href")).toBe("/env/e1/containers/proxy-id?node=n1");
    for (const label of ["Stop", "Restart", "Pause", "Kill…", "Remove…"]) {
      const item = screen.getByRole("menuitem", { name: label });
      expect(item.getAttribute("aria-disabled"), label).toBe("true");
    }
    expect(screen.getAllByText(REASON).length).toBeGreaterThan(0);
  });

  it("opens no dialog and sends nothing when a disabled action is chosen", async () => {
    mountRow(row({ state: "running" }));
    await openMenu("tierplan-ingress-proxy-1");
    fireEvent.click(await screen.findByRole("menuitem", { name: "Stop" }));
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(writes()).toEqual([]);
  });

  it("disables Start on a stopped proxy and Unpause on a paused one", async () => {
    const view = mountRow(row({ state: "exited" }));
    await openMenu("tierplan-ingress-proxy-1");
    expect(
      (await screen.findByRole("menuitem", { name: "Start" })).getAttribute("aria-disabled"),
    ).toBe("true");
    view.unmount();
    cleanup();
    mountRow(row({ state: "paused" }));
    await openMenu("tierplan-ingress-proxy-1");
    expect(
      (await screen.findByRole("menuitem", { name: "Unpause" })).getAttribute("aria-disabled"),
    ).toBe("true");
    expect(writes()).toEqual([]);
  });

  it("keeps the typed-name flow for a swarm proxy task that the server does not own", async () => {
    mountRow(row({ ingressOwned: false, name: "proxy.1.abc", service: "proxy" }));
    await openMenu("proxy.1.abc");
    const stop = await screen.findByRole("menuitem", { name: "Stop" });
    expect(stop.getAttribute("aria-disabled")).toBeNull();
    fireEvent.click(stop);
    const dialog = await screen.findByRole("alertdialog", { name: "Stop proxy.1.abc?" });
    expect(within(dialog).getByLabelText("Type proxy.1.abc to confirm")).toBeTruthy();
    expect(writes()).toEqual([]);
  });
});

describe("TestStandaloneProxyPageIsReadOnly", () => {
  it("disables the header actions with the reason and sends nothing", async () => {
    renderAt(PAGE);
    await screen.findByRole("heading", { name: "tierplan-ingress-proxy-1" });
    for (const label of ["Stop", "Restart"]) {
      const button = await screen.findByRole("button", { name: label });
      expect(button.getAttribute("aria-disabled"), label).toBe("true");
      fireEvent.click(button);
    }
    expect(screen.queryByRole("alertdialog")).toBeNull();
    // The rest sit under More, off as well.
    const more = await screen.findByRole("button", { name: "More actions" });
    await act(async () => {
      fireEvent.keyDown(more, { key: "Enter" });
    });
    for (const item of await screen.findAllByRole("menuitem")) {
      expect(item.getAttribute("aria-disabled"), item.textContent ?? "").toBe("true");
    }
    expect(screen.getAllByText(REASON).length).toBeGreaterThan(0);
    expect(writes()).toEqual([]);
  });

  it("disables the Shell tab with the reason, and the shell page offers no launcher", async () => {
    renderAt(PAGE);
    await screen.findByRole("heading", { name: "tierplan-ingress-proxy-1" });
    const tab = await screen.findByRole("tab", { name: "Shell" });
    await waitFor(() => expect(tab.getAttribute("aria-disabled")).toBe("true"));
    expect(document.body.textContent).toContain(REASON);
    cleanup();

    renderAt(`/env/${ENV}/containers/proxy-id/shell?node=n1`);
    await screen.findByRole("heading", { name: "tierplan-ingress-proxy-1" });
    await waitFor(() => expect(screen.getAllByText(REASON).length).toBeGreaterThan(1));
    expect(screen.queryByRole("button", { name: /open shell|start shell/i })).toBeNull();
    expect(writes()).toEqual([]);
  });

  it("disables Connect to network and Disconnect on the container's networks", async () => {
    renderAt(PAGE);
    await screen.findByRole("heading", { name: "tierplan-ingress-proxy-1" });
    const connect = await screen.findByRole("button", { name: "Connect to network" });
    expect(connect.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(connect);
    const disconnect = await screen.findByRole("button", {
      name: `Disconnect tierplan-ingress-proxy-1 from ${BRIDGE}`,
    });
    expect(disconnect.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(disconnect);
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(writes()).toEqual([]);
  });

  it("does not offer the proxy in a network's connect dialog, and still offers other containers", async () => {
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
                scope: "local",
                nodeId: "n1",
                attachable: false,
              }}
            />
          </TooltipProvider>
        </TestRouter>
      </QueryClientProvider>,
    );
    const dialog = await screen.findByRole("dialog");
    const box = await within(dialog).findByRole("combobox", { name: "Container" });
    fireEvent.click(box);
    const options = (await screen.findAllByRole("option")).map((o) => o.textContent ?? "");
    expect(options.some((o) => o.startsWith("web"))).toBe(true);
    expect(options.some((o) => o.startsWith("tierplan-ingress-proxy-1"))).toBe(false);
  });

  it("says the same sentence when a stale page's action is answered ingress_owned", async () => {
    actionAnswer = () =>
      Response.json({
        results: [
          {
            nodeId: "n1",
            id: "proxy-id",
            name: "tierplan-ingress-proxy-1",
            result: "failed",
            code: "ingress_owned",
          },
        ],
      });
    // A page loaded before the proxy was known to be TierPlan's offers Restart as for any container.
    mountRow(row({ ingressOwned: false, selfKind: "" }));
    await openMenu("tierplan-ingress-proxy-1");
    fireEvent.click(await screen.findByRole("menuitem", { name: "Restart" }));
    await waitFor(() => expect(writes()).toHaveLength(1));
    expect((await screen.findAllByText(new RegExp(REASON.replace(/[.]/g, "\\.")))).length).toBe(1);
  });
});

describe("TestStandaloneIngressNetworkAndVolumeRowsShowTheBadge", () => {
  it("badges the local bridge network, offers no delete and carries the owned reason", async () => {
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
    const link = await screen.findByRole("link", { name: BRIDGE });
    const tr = link.closest("tr") as HTMLElement;
    expect(within(tr).getByText("TierPlan ingress")).toBeTruthy();
    expect(within(tr).getByText("Local")).toBeTruthy();
    const del = within(tr).getByRole("button", { name: `Delete network ${BRIDGE}` });
    expect(del.getAttribute("aria-disabled")).toBe("true");
    fireEvent.focus(del);
    expect((await screen.findAllByText(OWNED_REASON)).length).toBeGreaterThan(0);
    fireEvent.click(del);
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(
      within(tr).getByRole("link", { name: new RegExp(`Open network ${BRIDGE}`) }),
    ).toBeTruthy();
    // A plain local network beside it keeps Delete.
    const plain = (await screen.findByRole("link", { name: "app-net" })).closest(
      "tr",
    ) as HTMLElement;
    expect(within(plain).queryByText("TierPlan ingress")).toBeNull();
    expect(
      within(plain)
        .getByRole("button", { name: "Delete network app-net" })
        .getAttribute("aria-disabled"),
    ).toBeNull();
    expect(writes()).toEqual([]);
  });

  it("badges the cache volume, offers no delete, and a bulk that selects it deletes nothing of it", async () => {
    render(
      <QueryClientProvider client={client}>
        <TestRouter>
          <TooltipProvider>
            <VolumeList envId={ENV} search={{}} onSearchChange={() => {}} />
            <Toaster />
          </TooltipProvider>
        </TestRouter>
      </QueryClientProvider>,
    );
    const tr = (await screen.findByRole("link", { name: CACHE })).closest("tr") as HTMLElement;
    expect(within(tr).getByText("TierPlan ingress")).toBeTruthy();
    const del = within(tr).getByRole("button", { name: `Delete ${CACHE}` });
    expect(del.getAttribute("aria-disabled")).toBe("true");
    fireEvent.focus(del);
    expect((await screen.findAllByText(OWNED_REASON)).length).toBeGreaterThan(0);
    fireEvent.click(del);
    expect(screen.queryByRole("alertdialog")).toBeNull();

    for (const name of [CACHE, "a", "b"]) {
      fireEvent.click(await screen.findByRole("checkbox", { name: `Select ${name}` }));
    }
    fireEvent.click(await screen.findByRole("button", { name: "Delete…" }));
    const dialog = await screen.findByRole("alertdialog");
    fireEvent.change(within(dialog).getByLabelText("Type delete 2 volumes to confirm"), {
      target: { value: "delete 2 volumes" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete 2 volumes" }));
    await waitFor(() => expect(writes()).toHaveLength(1));
    expect(writes()[0]?.body?.targets).toEqual([
      { nodeId: "node-1-id", name: "a" },
      { nodeId: "node-1-id", name: "b" },
    ]);
  });
});
