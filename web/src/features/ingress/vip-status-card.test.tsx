// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import type { IngressVIP } from "@/api/ingress-nodes";
import { TooltipProvider } from "@/components/ui/tooltip";
import { vipFixture } from "@/features/ingress/vip-fixture";
import { VIPStatusCard } from "@/features/ingress/vip-status-card";
import "@/i18n";

// The hex of the VRRP key the fixture stands for: the card never has it, and no view may draw anything derived from it.
const KEY_HEX = "9f3a1c5e7b2d4f60a8c1e3b5d7f90214";

afterEach(cleanup);

function card(over: Partial<IngressVIP> = {}) {
  return render(
    <TooltipProvider>
      <VIPStatusCard vip={vipFixture(over)} />
    </TooltipProvider>,
  );
}

describe("the virtual IP status card: holder and facts", () => {
  it("names the holder, draws the address and router ID in mono and the key generation as a number", () => {
    const { container } = card();
    expect(screen.getByRole("heading", { name: "Virtual IP" })).toBeTruthy();
    expect(screen.getByText("Held by node-b")).toBeTruthy();
    expect(screen.getByText("192.168.10.250/24").className).toContain("font-mono");
    expect(screen.getByText("51").className).toContain("font-mono");
    expect(screen.getByText("chosen by TierPlan")).toBeTruthy();
    const generation = screen.getByText("Key generation").nextElementSibling;
    expect(generation?.textContent).toBe("3");
    // Never the key, nor anything of it.
    expect(container.textContent).not.toContain(KEY_HEX);
    expect(container.textContent).not.toMatch(/[0-9a-f]{16,}/);
  });

  it("says nothing of who chose the router ID when an Admin did", () => {
    card({ routerDerived: false, routerId: 77 });
    expect(screen.queryByText("chosen by TierPlan")).toBeNull();
    expect(screen.getByText("77")).toBeTruthy();
  });

  it("says no node holds the address, and since when", () => {
    card({ holder: { state: "none", nodes: [] }, holderlessSince: "2026-11-20T09:00:00Z" });
    expect(screen.getByText("No node holds the address")).toBeTruthy();
    expect(screen.getByText(/^Since/)).toBeTruthy();
    expect(document.querySelector("time[datetime='2026-11-20T09:00:00Z']")).not.toBeNull();
  });

  it("draws no Since line for a holderless address TierPlan has not read yet", () => {
    card({ holder: { state: "none", nodes: [] } });
    expect(screen.queryByText(/^Since/)).toBeNull();
  });

  it("names every holder when more than one node holds the address", () => {
    card({ holder: { state: "many", nodes: ["sw-a", "sw-b"] } });
    expect(screen.getByText("More than one node holds the address")).toBeTruthy();
    expect(screen.getByText("node-a, node-b")).toBeTruthy();
  });

  it("falls back to the node ID for a holder no list names", () => {
    card({ holder: { state: "one", nodes: ["sw-z"] } });
    expect(screen.getByText("Held by sw-z")).toBeTruthy();
  });

  it("writes a failed last check with its error in the warning colour, never red", () => {
    card({ lastEnsure: { at: "2026-11-20T10:00:00Z", error: "service_update_failed" } });
    const text = screen.getByText(/^Failed/);
    expect(text.textContent).toContain("service_update_failed");
    const cell = text.closest("dd") as HTMLElement;
    expect(cell.className).toContain("text-status-warning-text");
    expect(cell.className).not.toMatch(/destructive|offline/);
  });

  it("writes a passing last check as the time alone", () => {
    card();
    const cell = screen.getByText("Last check").nextElementSibling as HTMLElement;
    expect(within(cell).queryByText(/Failed/)).toBeNull();
    expect(cell.querySelector("time[datetime='2026-11-20T10:00:00Z']")).not.toBeNull();
    expect(cell.className).not.toContain("text-status-warning-text");
  });
});
