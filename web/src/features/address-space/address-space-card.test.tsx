// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { EnvironmentItem } from "@/api/environments";
import { TooltipProvider } from "@/components/ui/tooltip";
import { AddressSpaceCard } from "@/features/address-space/address-space-card";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

type Summary = NonNullable<EnvironmentItem["addressSpace"]>;

const POOL: Summary = {
  worstName: "10.0.0.0/16",
  worstKind: "pool",
  used: 213,
  capacity: 256,
  percent: 83,
  state: "getting_full",
  unit: "subnets",
  estimated: false,
  orphaned: 3,
};

let me: ReturnType<typeof makeMe>;
let client: QueryClient;

beforeEach(() => {
  setCsrfToken(null);
  me = makeMe({ envs: { e1: "viewer" } });
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      if (new URL(req.url).pathname === "/api/me") return Response.json(me);
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  client.clear();
});

function mount(props: Partial<React.ComponentProps<typeof AddressSpaceCard>> = {}) {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <AddressSpaceCard envId="e1" summary={POOL} {...props} />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

describe("AddressSpaceCard", () => {
  it("shows the worst meter and orphan count", async () => {
    mount();
    const card = await screen.findByRole("region", { name: "Address space" });
    expect(within(card).getByText("10.0.0.0/16").className).toContain("font-mono");
    const meter = within(card).getByRole("meter", { name: "10.0.0.0/16" });
    expect(meter.getAttribute("aria-valuenow")).toBe("213");
    expect(within(card).getByText("83%")).toBeTruthy();
    expect(within(card).getByText("213 of 256 subnets used")).toBeTruthy();
    const orphans = within(card).getByRole("link", { name: "3 orphaned networks" });
    expect(orphans.getAttribute("href")).toBe("/env/e1/address-space#orphans");
    expect(
      within(card).getByRole("link", { name: "Open address space" }).getAttribute("href"),
    ).toBe("/env/e1/address-space");
  });

  it("names an overlay in the body font and leaves out the orphan link at none", async () => {
    mount({
      summary: {
        ...POOL,
        worstName: "shop_default",
        worstKind: "overlay",
        unit: "addresses",
        used: 27,
        capacity: 29,
        percent: 93,
        state: "almost_full",
        orphaned: 0,
      },
    });
    const card = await screen.findByRole("region", { name: "Address space" });
    expect(within(card).getByText("shop_default").className).not.toContain("font-mono");
    expect(within(card).getByText("27 of 29 addresses used")).toBeTruthy();
    expect(within(card).queryByText(/orphaned/)).toBeNull();
  });

  it("marks an estimate", async () => {
    mount({ summary: { ...POOL, estimated: true, percent: 92, used: 236, state: "almost_full" } });
    const card = await screen.findByRole("region", { name: "Address space" });
    expect(within(card).getByText("Estimated")).toBeTruthy();
    expect(
      within(card).getByText("Estimated, so the environment's health isn't affected."),
    ).toBeTruthy();
  });

  it("does not repeat the estimate note below 90 percent", async () => {
    mount({ summary: { ...POOL, estimated: true } });
    const card = await screen.findByRole("region", { name: "Address space" });
    expect(within(card).getByText("Estimated")).toBeTruthy();
    expect(within(card).queryByText(/health isn't affected/)).toBeNull();
  });

  it("shows not measured yet", async () => {
    mount({ summary: null });
    const card = await screen.findByRole("region", { name: "Address space" });
    expect(within(card).getByText("Not measured yet")).toBeTruthy();
    expect(
      within(card).getByText(
        "Coaming reads the pools from a manager, or estimates them on a standalone host.",
      ),
    ).toBeTruthy();
    expect(within(card).getByRole("link", { name: "Open address space" })).toBeTruthy();
  });

  it("shows a skeleton while loading and an error with the link", async () => {
    mount({ loading: true, summary: undefined });
    const card = await screen.findByRole("region", { name: "Address space" });
    expect(card.getAttribute("aria-busy")).toBe("true");
    cleanup();
    mount({ error: true, summary: undefined });
    const failed = await screen.findByRole("region", { name: "Address space" });
    expect(within(failed).getByText("Couldn't read the address space.")).toBeTruthy();
    expect(within(failed).getByRole("link", { name: "Open address space" })).toBeTruthy();
  });

  it("is hidden without networks.read", async () => {
    me = makeMe({
      permissions: { global: [], environments: { e1: ["containers.read"] } },
    });
    mount();
    // Who the person is arrives with /api/me; the card stays hidden until it says they may read.
    await waitFor(() => expect(vi.mocked(fetch)).toHaveBeenCalled());
    await new Promise((r) => setTimeout(r, 25));
    expect(screen.queryByRole("region", { name: "Address space" })).toBeNull();
  });
});
