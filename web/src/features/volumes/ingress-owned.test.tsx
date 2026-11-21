// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { detailRetry } from "@/api/container-detail";
import { queryClient } from "@/api/query-client";
import type { VolumeDetail } from "@/api/volumes";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { VolumeList } from "@/features/volumes/volume-list";
import { routeTree } from "@/routeTree.gen";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// The ingress cache volume is TierPlan's (UI-SPEC S3, D-29, DEP-08): badged, read-only with
// the reason, and skipped, not sent, in a bulk delete.

const ENV = "e1";
const CACHE = "tierplan-ingress-cache";
const REASON = "Managed by TierPlan. Switch ingress off to remove it.";

interface Sent {
  method: string;
  path: string;
  body?: { confirm?: string; targets?: { nodeId: string; name: string }[] };
}

let sent: Sent[];
let client: QueryClient;
let volumes: unknown[];
let detail: VolumeDetail;

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

// The cache is mounted by the proxy, so it always reads as in use.
const cache = () => volume(CACHE, { ingress: true, usedByContainers: 1 });

function detailOf(extra: Partial<VolumeDetail> = {}): VolumeDetail {
  return {
    name: CACHE,
    driver: "local",
    mountpoint: `/var/lib/docker/volumes/${CACHE}/_data`,
    scope: "local",
    node: "node-1",
    nodeId: "n1",
    created: "2026-10-01T10:00:00Z",
    anonymous: false,
    ingress: true,
    options: [],
    labels: {},
    usedBy: [
      {
        kind: "container",
        id: "c1",
        name: "proxy.1.abc",
        node: "node-1",
        nodeId: "n1",
        mountedAt: "/data",
        readOnly: false,
        state: "running",
      },
    ],
    usedByContainers: 1,
    usedByServices: 0,
    notReporting: [],
    ...extra,
  };
}

beforeEach(() => {
  sent = [];
  volumes = [cache(), volume("a"), volume("b")];
  detail = detailOf();
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
      const entry: Sent = {
        method: req.method,
        path: url.pathname,
        body: req.method === "POST" ? await req.clone().json() : undefined,
      };
      sent.push(entry);
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
              containers: 1,
              containersRunning: 1,
              containersStopped: 0,
              images: 0,
              volumes: 3,
              networks: 0,
            },
            isWorker: false,
            fetchedAt: "2026-10-01T10:00:00Z",
          });
        case `/api/env/${ENV}/volumes/${CACHE}`:
          return Response.json(detail);
        case `/api/env/${ENV}/volumes/delete`:
          return Response.json({
            results: (entry.body?.targets ?? []).map((t) => ({ ...t, result: "done" })),
          });
        case `/api/env/${ENV}/volumes`:
          return Response.json({
            items: volumes,
            total: volumes.length,
            truncated: false,
            nodes: ["node-1"],
            notReporting: [],
            unfilteredTotal: volumes.length,
            usageCounts: { all: volumes.length, used: 1, unused: volumes.length - 1 },
          });
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

const deletes = () => sent.filter((s) => s.path.endsWith("/volumes/delete"));

function mountList() {
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

describe("the volumes list", () => {
  it("badges the cache volume and disables Delete with the reason", async () => {
    mountList();
    const row = (await screen.findByRole("link", { name: CACHE })).closest("tr") as HTMLElement;
    expect(within(row).getByText("TierPlan ingress")).toBeTruthy();
    const del = within(row).getByRole("button", { name: `Delete ${CACHE}` });
    expect(del.getAttribute("aria-disabled")).toBe("true");
    fireEvent.focus(del);
    expect((await screen.findAllByText(REASON)).length).toBeGreaterThan(0);
    fireEvent.click(del);
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(deletes()).toEqual([]);
    expect(within(row).getByRole("link", { name: `Open volume ${CACHE}` })).toBeTruthy();
  });

  it("leaves a plain volume without the badge and with Delete open", async () => {
    mountList();
    const row = (await screen.findByRole("link", { name: "a" })).closest("tr") as HTMLElement;
    expect(within(row).queryByText("TierPlan ingress")).toBeNull();
    expect(
      within(row).getByRole("button", { name: "Delete a" }).getAttribute("aria-disabled"),
    ).toBeNull();
  });

  it("lists the cache volume as skipped in a bulk delete and sends only the others", async () => {
    mountList();
    for (const name of [CACHE, "a", "b"]) {
      fireEvent.click(await screen.findByRole("checkbox", { name: `Select ${name}` }));
    }
    fireEvent.click(await screen.findByRole("button", { name: "Delete…" }));
    const dialog = await screen.findByRole("alertdialog");
    // Two volumes are acted on; the cache is only listed.
    expect(within(dialog).getByText("Delete 2 volumes?")).toBeTruthy();
    const list = dialog.querySelector('[data-slot="target-list"]') as HTMLElement;
    const skipped = within(list).getByText(CACHE).closest("tr") as HTMLElement;
    expect(skipped.getAttribute("data-skipped")).toBe("true");
    expect(within(skipped).getByText("Managed by TierPlan ingress")).toBeTruthy();

    fireEvent.change(within(dialog).getByLabelText("Type delete 2 volumes to confirm"), {
      target: { value: "delete 2 volumes" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete 2 volumes" }));
    await waitFor(() => expect(deletes()).toHaveLength(1));
    expect(deletes()[0]?.body?.targets).toEqual([
      { nodeId: "node-1-id", name: "a" },
      { nodeId: "node-1-id", name: "b" },
    ]);
    expect(await screen.findByText("Deleted 2 volumes.")).toBeTruthy();
  });

  it("disables the bulk Delete with the reason when only the cache volume is selected", async () => {
    mountList();
    fireEvent.click(await screen.findByRole("checkbox", { name: `Select ${CACHE}` }));
    const bulk = await screen.findByRole("button", { name: "Delete…" });
    expect(bulk.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(bulk);
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(deletes()).toEqual([]);
  });
});

describe("the cache volume's page", () => {
  it("draws the badge and the neutral notice and disables Delete with the reason", async () => {
    renderAt(`/env/${ENV}/volumes/${CACHE}?node=n1`);
    await screen.findByRole("heading", { level: 2, name: CACHE });
    expect(screen.getByText("TierPlan ingress")).toBeTruthy();
    const notice = screen.getByText("Managed by TierPlan ingress").closest("[role]") as HTMLElement;
    expect(notice.getAttribute("role")).toBe("status");
    expect(
      within(notice).getByText(
        "TierPlan creates and removes this itself. To remove it, switch ingress off.",
      ),
    ).toBeTruthy();
    // Mounted by the proxy, but the in-use warning is not the reason it cannot be deleted.
    expect(screen.queryByText(`${CACHE} is in use`)).toBeNull();
    const del = screen.getByRole("button", { name: `Delete ${CACHE}` });
    expect(del.getAttribute("aria-disabled")).toBe("true");
    fireEvent.focus(del);
    expect((await screen.findAllByText(REASON)).length).toBeGreaterThan(0);
    fireEvent.click(del);
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(screen.getByRole("tab", { name: "Inspect" }).getAttribute("aria-disabled")).toBeNull();
    expect(deletes()).toEqual([]);
  });

  it("shows no badge and no notice on a plain volume", async () => {
    detail = detailOf({ ingress: false, usedBy: [], usedByContainers: 0 });
    renderAt(`/env/${ENV}/volumes/${CACHE}?node=n1`);
    await screen.findByRole("heading", { level: 2, name: CACHE });
    expect(screen.queryByText("TierPlan ingress")).toBeNull();
    expect(screen.queryByText("Managed by TierPlan ingress")).toBeNull();
  });
});
