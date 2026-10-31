// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { AddressPool, AddressSpace, OverlayUsage } from "@/api/address-space";
import { setCsrfToken } from "@/api/client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { AddressSpacePage } from "@/features/address-space/address-space-page";
import "@/i18n";
import { makeMe } from "@/test-me";
import { splitPermissions } from "@/test-permissions";
import { TestRouter } from "@/test-router";

const ENV = "e1";

let body: AddressSpace;
// What the stubbed server answers for the address space: a body, a failure or nothing at all.
let answer: () => Response | Promise<Response>;
let reads: number;
let me: ReturnType<typeof makeMe>;
let client: QueryClient;

const pool = (extra: Partial<AddressPool> = {}): AddressPool => ({
  cidr: "10.0.0.0/8",
  capacity: 255,
  used: 43,
  percent: 16,
  mask: 24,
  state: "normal",
  estimated: false,
  source: "swarm",
  largestFree: "10.128.0.0/9",
  largestFreeSubnets: 32768,
  hostRouteBlocks: 0,
  ...extra,
});

const overlay = (name: string, extra: Partial<OverlayUsage> = {}): OverlayUsage => ({
  id: `id-${name}`,
  name,
  ingress: false,
  measured: true,
  outsidePool: false,
  subnets: ["10.0.1.0/24"],
  capacity: 256,
  used: 20,
  percent: 7,
  state: "normal",
  ...extra,
});

const space = (extra: Partial<AddressSpace> = {}): AddressSpace => ({
  envType: "swarm",
  measured: true,
  networks: [],
  networksTotal: 0,
  networksTruncated: false,
  notReporting: [],
  orphans: [],
  kept: [],
  overlays: [],
  pools: [pool()],
  ...extra,
});

beforeEach(() => {
  setCsrfToken(null);
  me = makeMe({ envs: { [ENV]: "admin" } });
  body = space();
  reads = 0;
  answer = () => Response.json(body);
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      if (url.pathname === "/api/me") return Response.json(me);
      if (url.pathname === `/api/env/${ENV}/address-space`) {
        reads++;
        return answer();
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
  // A page mounted twice in one test must not start from the first one's answer.
  client.clear();
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <AddressSpacePage envId={ENV} search={{}} onSearchChange={() => {}} />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

describe("the Address space page", () => {
  it("renders the pool rows", async () => {
    mount();
    expect(await screen.findByRole("heading", { name: "10.0.0.0/8" })).toBeTruthy();
    expect(
      screen.getByText(
        "43 of 255 subnets used · each network gets a /24 · largest free block 10.128.0.0/9 (32,768 subnets)",
      ),
    ).toBeTruthy();
    expect(screen.getByText("Measured from the swarm.")).toBeTruthy();
    const meter = screen.getByRole("meter", { name: "10.0.0.0/8" });
    expect(meter.getAttribute("aria-valuetext")).toBe("43 of 255 subnets used, 16 percent");
    expect(screen.getByRole("heading", { level: 1, name: "Address space" })).toBeTruthy();
  });

  it("shows the summary only from 70 percent", async () => {
    body = space({ pools: [pool({ used: 100, percent: 39 })] });
    mount();
    await screen.findByRole("heading", { name: "10.0.0.0/8" });
    expect(screen.queryByText("Address space is getting full")).toBeNull();
    expect(screen.queryByText("Address space is almost full")).toBeNull();
    cleanup();

    body = space({ pools: [pool({ used: 180, percent: 70, state: "getting_full" })] });
    mount();
    const gettingFull = await screen.findByRole("status");
    expect(within(gettingFull).getByText("Address space is getting full")).toBeTruthy();
    expect(
      within(gettingFull).getByText(
        "10.0.0.0/8 is 70% used. New stacks may fail to deploy when it runs out.",
      ),
    ).toBeTruthy();
    cleanup();

    body = space({
      pools: [pool({ used: 180, percent: 70, state: "getting_full" })],
      overlays: [overlay("web-net", { used: 240, percent: 93, state: "almost_full" })],
    });
    mount();
    const almostFull = await screen.findByRole("status");
    expect(within(almostFull).getByText("Address space is almost full")).toBeTruthy();
    expect(within(almostFull).getByText(/^web-net is 93% used\./)).toBeTruthy();
    // It is a polite status, never an alert.
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("words the estimated note by source", async () => {
    const notes = {
      daemon:
        "These numbers are estimated from the address pools the daemon reports and the bridge subnets Coaming can see.",
      defaults:
        "These numbers are estimated. The daemon doesn't set its own address pools, so Coaming counts against Docker's default pools and the bridge subnets it can see.",
      entered:
        "These numbers are estimated from the pools you entered in this environment's settings and the bridge subnets Coaming can see.",
    } as const;
    for (const source of ["daemon", "defaults", "entered"] as const) {
      body = space({
        envType: "standalone",
        measured: false,
        pools: [pool({ source, estimated: true, cidr: "172.17.0.0/16", mask: 24 })],
      });
      mount();
      expect(await screen.findByText(notes[source])).toBeTruthy();
      expect(screen.getByText("Estimated")).toBeTruthy();
      // The old sentence is gone.
      expect(screen.queryByText(/Docker doesn't tell Coaming which address pools/)).toBeNull();
      // A standalone page has no swarm caption.
      expect(screen.queryByText("Measured from the swarm.")).toBeNull();
      cleanup();
    }
    // No pool at all counts against the defaults.
    body = space({ envType: "standalone", measured: false, pools: [] });
    mount();
    expect(await screen.findByText(notes.defaults)).toBeTruthy();
    expect(screen.getByText("No pools to show.")).toBeTruthy();
  });

  it("says an estimate at 90 percent never changes the environment's health", async () => {
    body = space({
      envType: "standalone",
      measured: false,
      pools: [
        pool({ estimated: true, source: "daemon", used: 240, percent: 94, state: "almost_full" }),
      ],
    });
    mount();
    expect(
      await screen.findByText("Estimated, so the environment's health isn't affected."),
    ).toBeTruthy();
    cleanup();
    body = space({
      envType: "standalone",
      measured: false,
      pools: [
        pool({ estimated: true, source: "daemon", used: 180, percent: 70, state: "getting_full" }),
      ],
    });
    mount();
    await screen.findByRole("heading", { name: "10.0.0.0/8" });
    expect(screen.queryByText("Estimated, so the environment's health isn't affected.")).toBeNull();
  });

  it("shows the no-manager alert", async () => {
    body = space({
      measured: false,
      notMeasured: "no_manager",
      pools: [pool({ state: "unknown", used: 0, percent: 0 })],
      ingress: overlay("ingress", {
        ingress: true,
        measured: false,
        state: "unknown",
        used: 0,
        capacity: 0,
      }),
    });
    mount();
    const alert = await screen.findByRole("status");
    expect(within(alert).getByText("Coaming can't measure the swarm's pools")).toBeTruthy();
    expect(
      within(alert).getByText(
        "No manager is connected. The subnets below are the last ones read from the nodes that are reporting.",
      ),
    ).toBeTruthy();
    expect(screen.queryByRole("meter")).toBeNull();
    expect(screen.getAllByText("Not measured").length).toBe(2);
    expect(screen.queryByText("Measured from the swarm.")).toBeNull();
  });

  it("marks an outside-the-pool ingress and leaves an ingress inside a pool alone", async () => {
    body = space({
      ingress: overlay("ingress", {
        ingress: true,
        outsidePool: true,
        subnets: ["192.168.99.0/24"],
        used: 12,
        capacity: 256,
        percent: 4,
      }),
    });
    mount();
    const row = await screen.findByRole("region", { name: "Ingress" });
    expect(within(row).getByText("192.168.99.0/24")).toBeTruthy();
    expect(within(row).getByText("12 of 256 addresses used")).toBeTruthy();
    const badge = within(row).getByText("Outside the pool");
    fireEvent.focus(badge);
    expect(
      (
        await screen.findAllByText(
          "Ingress sits outside the default pool, so it has its own meter.",
        )
      ).length,
    ).toBeGreaterThan(0);
    cleanup();

    body = space({
      ingress: overlay("ingress", { ingress: true, outsidePool: false, subnets: ["10.0.0.0/24"] }),
    });
    mount();
    const inside = await screen.findByRole("region", { name: "Ingress" });
    expect(within(inside).queryByText("Outside the pool")).toBeNull();
  });

  it("reads an unmeasured ingress as Not measured, with the Engine 29 explanation", async () => {
    body = space({
      ingress: overlay("ingress", {
        ingress: true,
        measured: false,
        state: "unknown",
        used: 0,
        capacity: 0,
      }),
    });
    mount();
    // The pool meter still renders.
    expect(await screen.findByRole("meter", { name: "10.0.0.0/8" })).toBeTruthy();
    const row = screen.getByRole("region", { name: "Ingress" });
    const word = within(row).getByText("Not measured");
    fireEvent.focus(word);
    expect(
      (
        await screen.findAllByText(
          "Counting used addresses needs Docker Engine 29 or newer on the manager.",
        )
      ).length,
    ).toBeGreaterThan(0);
  });

  it("lists filling networks worst first, each name one link to its page", async () => {
    body = space({
      overlays: [
        overlay("calm", { used: 20, percent: 7 }),
        overlay("mid", { used: 190, percent: 74, state: "getting_full", subnets: ["10.0.2.0/24"] }),
        overlay("worst", {
          used: 250,
          percent: 97,
          state: "almost_full",
          subnets: ["10.0.3.0/24"],
        }),
        overlay("unread", { measured: false, state: "unknown", used: 0, capacity: 0 }),
      ],
    });
    mount();
    expect(await screen.findByRole("heading", { name: "Networks filling up" })).toBeTruthy();
    const links = screen
      .getAllByRole("link")
      .map((l) => l.textContent)
      .filter((n) => ["calm", "mid", "worst", "unread"].includes(n ?? ""));
    expect(links).toEqual(["worst", "mid"]);
    const worst = screen.getByRole("region", { name: "worst" });
    expect(within(worst).getByRole("link", { name: "worst" }).getAttribute("href")).toBe(
      "/env/e1/networks/id-worst",
    );
    expect(within(worst).getAllByRole("link").length).toBe(1);
    expect(within(worst).getByText("10.0.3.0/24")).toBeTruthy();
    expect(within(worst).getByText("250 of 256 addresses used")).toBeTruthy();
    expect(within(worst).getByText("97%")).toBeTruthy();
    expect(within(worst).getByText("Almost full")).toBeTruthy();
  });

  it("leaves the filling group out when no overlay is at 70 percent", async () => {
    body = space({ overlays: [overlay("calm")] });
    mount();
    await screen.findByRole("heading", { name: "10.0.0.0/8" });
    expect(screen.queryByRole("heading", { name: "Networks filling up" })).toBeNull();
  });

  it("renders the text of a host as text, never as markup", async () => {
    const name = "<img src=x onerror=alert(1)>";
    body = space({ overlays: [overlay(name, { used: 250, percent: 97, state: "almost_full" })] });
    mount();
    const link = await screen.findByRole("link", { name });
    expect(link.textContent).toBe(name);
    expect(document.querySelector("img")).toBeNull();
  });

  it("names the nodes that did not report", async () => {
    body = space({ notReporting: ["worker-3"] });
    mount();
    expect(await screen.findByText(/worker-3/)).toBeTruthy();
  });

  it("scrolls inside the card past 8 pools and not before", async () => {
    const many = (n: number) =>
      Array.from({ length: n }, (_, i) => pool({ cidr: `10.${i}.0.0/16`, mask: 24 }));
    body = space({ pools: many(8) });
    mount();
    await screen.findByRole("heading", { name: "10.0.0.0/16" });
    expect(document.querySelector(".max-h-96")).toBeNull();
    cleanup();
    body = space({ pools: many(9) });
    mount();
    await screen.findByRole("heading", { name: "10.0.0.0/16" });
    expect(document.querySelector(".max-h-96")).not.toBeNull();
  });

  it("says a pool with no free block left has none", async () => {
    body = space({
      pools: [
        pool({
          used: 255,
          percent: 100,
          state: "almost_full",
          largestFree: "",
          largestFreeSubnets: 0,
        }),
      ],
    });
    mount();
    expect(
      await screen.findByText(
        "255 of 255 subnets used · each network gets a /24 · no free /24 block left",
      ),
    ).toBeTruthy();
  });

  it("renders the loading skeleton with the title at once", async () => {
    answer = () => new Promise<Response>(() => {});
    mount();
    expect(await screen.findByRole("heading", { level: 1, name: "Address space" })).toBeTruthy();
    expect(await screen.findByText("Pools")).toBeTruthy();
    await waitFor(() => expect(screen.getAllByTestId("meter-skeleton").length).toBe(3));
    expect(screen.queryByRole("meter")).toBeNull();
  });

  it("renders the error with retry", async () => {
    answer = () => new Response("nope", { status: 500 });
    mount();
    expect(await screen.findByText("Couldn't load the address space.")).toBeTruthy();
    expect(screen.getByText("Check the connection and try again.")).toBeTruthy();
    const before = reads;
    answer = () => Response.json(body);
    fireEvent.click(screen.getByRole("button", { name: "Retry loading address space" }));
    expect(await screen.findByRole("heading", { name: "10.0.0.0/8" })).toBeTruthy();
    expect(reads).toBeGreaterThan(before);
    expect(screen.queryByText("Couldn't load the address space.")).toBeNull();
  });

  it("shows NoPermission and reads nothing for a user without networks.read", async () => {
    me = makeMe({
      envs: { [ENV]: "viewer" },
      permissions: splitPermissions(["environments.read"]),
    });
    mount();
    await screen.findByText(/permission/i);
    expect(reads).toBe(0);
    expect(screen.queryByRole("meter")).toBeNull();
  });

  it("shows the orphans card and the kept list under the map", async () => {
    body = space({
      orphans: [
        {
          class: "orphaned",
          formerOwner: "shop",
          formerOwnerKind: "stack",
          idleSince: "2026-10-01T10:00:00Z",
          idleSinceSource: "seen",
          name: "shop_default",
          networkId: "n1",
          node: "Cluster",
          nodeId: "m1",
          scope: "swarm",
          subnets: ["10.0.9.0/24"],
        },
      ],
      kept: [
        {
          keptBy: "user",
          keptByName: "ian",
          keptAt: "2026-10-02T10:00:00Z",
          name: "backend",
          node: "Cluster",
          nodeId: "",
          present: true,
          subnets: [],
        },
      ],
    });
    mount();
    const card = await screen.findByText("Orphaned (1)");
    expect(card).toBeTruthy();
    expect(document.getElementById("orphans")?.contains(card)).toBe(true);
    expect(screen.getByRole("button", { name: "Kept networks (1)" })).toBeTruthy();
  });
});
