// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { ContainerRow } from "@/api/resources";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { ContainerRowActions } from "@/features/containers/row-actions";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// The standalone ingress proxy is TierPlan's and read-only wherever the web offers a container
// write (D-09): the list row, its page, the shell, the network dialogs, and the bridge network and
// cache volume it owns. The server refuses regardless (25-08); these pin that the browser does not
// offer what will be refused.

const ENV = "e1";
const REASON = "TierPlan's ingress proxy for this host. Switch ingress off to remove it.";

interface Sent {
  method: string;
  path: string;
}

let sent: Sent[];
let client: QueryClient;

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
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      sent.push({ method: req.method, path });
      if (path === "/api/me") return Response.json(makeMe({ envs: { [ENV]: "admin" } }));
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
});

const writes = () => sent.filter((s) => s.method !== "GET");

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
