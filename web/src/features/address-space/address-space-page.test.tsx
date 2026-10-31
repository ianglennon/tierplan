// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { AddressPool, AddressSpace } from "@/api/address-space";
import { setCsrfToken } from "@/api/client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { AddressSpacePage } from "@/features/address-space/address-space-page";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

const ENV = "e1";

let body: AddressSpace;
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
  ...extra,
});

const space = (extra: Partial<AddressSpace> = {}): AddressSpace => ({
  envType: "swarm",
  measured: true,
  networks: [],
  networksTotal: 0,
  networksTruncated: false,
  notReporting: [],
  overlays: [],
  pools: [pool()],
  ...extra,
});

beforeEach(() => {
  setCsrfToken(null);
  me = makeMe({ envs: { [ENV]: "admin" } });
  body = space();
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      if (url.pathname === "/api/me") return Response.json(me);
      if (url.pathname === `/api/env/${ENV}/address-space`) return Response.json(body);
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
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <AddressSpacePage envId={ENV} />
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
});
