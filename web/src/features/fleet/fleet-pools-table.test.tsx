// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import type { FleetNotIncluded, FleetPoolItem } from "@/api/fleet";
import { TooltipProvider } from "@/components/ui/tooltip";
import { FleetPoolsTable } from "@/features/fleet/fleet-pools-table";
import { TestRouter } from "@/test-router";
import "@/i18n";

// The Address pools tab (plan 12-29, ENV-10, D-17): every pool and overlay network at 70% or more
// used, in the order the server gave.

function pool(extra: Partial<FleetPoolItem> = {}): FleetPoolItem {
  return {
    envId: "e1",
    envName: "prod",
    kind: "overlay",
    name: "shop_default",
    subnet: "10.0.3.0/24",
    used: 200,
    capacity: 254,
    percent: 78,
    state: "getting_full",
    unit: "addresses",
    estimated: false,
    ...extra,
  };
}

const MISSING: FleetNotIncluded = { envId: "e9", envName: "lab", reason: "offline", nodes: 0 };

afterEach(cleanup);

function mount(items: FleetPoolItem[], notIncluded: FleetNotIncluded[] = []) {
  render(
    <TooltipProvider>
      <TestRouter>
        <FleetPoolsTable items={items} notIncluded={notIncluded} />
      </TestRouter>
    </TooltipProvider>,
  );
}

const bodyRows = () => Array.from(document.querySelectorAll("tbody tr")) as HTMLElement[];

describe("FleetPoolsTable", () => {
  it("lists a pool with its environment, name, subnet, meter and state", () => {
    mount([pool()]);
    const table = screen.getByRole("table", { name: "Address pools near full" });
    for (const column of ["Environment", "Network or pool", "Subnet", "Addresses used", "State"]) {
      expect(within(table).getByRole("columnheader", { name: column })).toBeTruthy();
    }
    const row = bodyRows()[0] as HTMLElement;
    const env = within(row).getByRole("link", { name: "prod" });
    expect(env.getAttribute("href")).toBe("/env/e1/address-space");
    const name = within(row).getByText("shop_default");
    expect(name.className).toContain("font-mono");
    expect(within(row).getByText("10.0.3.0/24")).toBeTruthy();
    expect(within(row).getByText("200 / 254")).toBeTruthy();
    const meter = within(row).getByRole("meter", { name: "shop_default" });
    expect(meter.getAttribute("aria-valuenow")).toBe("200");
    // The percent is the server's, rounded down.
    expect(within(row).getByText("78%")).toBeTruthy();
    expect(within(row).getByText("Getting full")).toBeTruthy();
  });

  it("says Almost full for a pool at 90% or more", () => {
    mount([pool({ state: "almost_full", percent: 93, used: 236 })]);
    expect(within(bodyRows()[0] as HTMLElement).getByText("Almost full")).toBeTruthy();
    expect(within(bodyRows()[0] as HTMLElement).getByText("93%")).toBeTruthy();
  });

  it("keeps the order it was given", () => {
    mount([
      pool({ name: "b_net", percent: 95, state: "almost_full" }),
      pool({ name: "a_net", percent: 72 }),
      pool({ name: "c_net", percent: 72, envName: "zed", envId: "e2" }),
    ]);
    const names = bodyRows().map((r) => within(r).getByRole("meter").getAttribute("aria-label"));
    expect(names).toEqual(["b_net", "a_net", "c_net"]);
  });

  it("shows the estimated marker of a standalone host's guess", () => {
    mount([
      pool({ estimated: true, name: "172.17.0.0/16", unit: "subnets", used: 1, capacity: 1 }),
    ]);
    expect(within(bodyRows()[0] as HTMLElement).getByText("est.")).toBeTruthy();
  });

  it("draws names as text, never as markup", () => {
    mount([
      pool({ name: "<img src=x onerror=alert(1)>", envName: "<b>env</b>", subnet: "<i>s</i>" }),
    ]);
    const table = screen.getByRole("table");
    expect(within(table).getByText("<img src=x onerror=alert(1)>")).toBeTruthy();
    expect(within(table).getByText("<i>s</i>")).toBeTruthy();
    expect(table.querySelector("img, b, i")).toBeNull();
  });

  it("says No pool is getting full when nothing is missing", () => {
    mount([]);
    expect(screen.getByText("No pool is getting full")).toBeTruthy();
    expect(
      screen.getByText("Every overlay network and address pool is under 70% used."),
    ).toBeTruthy();
    expect(screen.queryByRole("table")).toBeNull();
    expect(screen.queryByRole("status")).toBeNull();
  });

  it("does not claim more than it saw when an environment did not report", () => {
    mount([], [MISSING]);
    expect(
      screen.getByText("No pool is getting full in the environments that reported"),
    ).toBeTruthy();
    expect(screen.queryByText("No pool is getting full")).toBeNull();
    expect(screen.getByRole("status").textContent).toContain("lab (offline)");
  });

  it("shows the coverage alert above a table that has rows", () => {
    mount([pool()], [MISSING]);
    expect(screen.getByRole("status").textContent).toContain("lab (offline)");
    expect(screen.getByRole("table")).toBeTruthy();
  });

  it("draws only the rows in view past 100 rows", () => {
    const items = Array.from({ length: 150 }, (_, i) =>
      pool({ name: `net_${String(i).padStart(3, "0")}` }),
    );
    mount(items);
    const table = screen.getByRole("table");
    expect(table.parentElement?.className).toContain("max-h-[70svh]");
    expect(bodyRows().length).toBeLessThan(150);
  });
});
