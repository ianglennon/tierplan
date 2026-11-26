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

describe("the virtual IP status card: warnings and managers", () => {
  it("draws no warning and no managers table that has nothing to say beyond the managers", () => {
    card();
    expect(screen.queryByText("Another router uses this router ID")).toBeNull();
    expect(screen.queryByText("A node's clock is out of step")).toBeNull();
    expect(screen.getByRole("table", { name: "Managers" })).toBeTruthy();
  });

  it("warns that another router uses the router ID, with the sender when one is reported", () => {
    card({ foreignRouter: { detected: true, sender: "192.168.10.7" } });
    const alert = screen.getByText("Another router uses this router ID").closest("[role=alert]");
    expect(alert?.textContent).toContain(
      "192.168.10.7 sends virtual router adverts with router ID 51 that this swarm's key doesn't match.",
    );
  });

  it("warns about a foreign router without a sender in the general sentence", () => {
    card({ foreignRouter: { detected: true } });
    const alert = screen.getByText("Another router uses this router ID").closest("[role=alert]");
    expect(alert?.textContent).toContain("Something on the network sends virtual router adverts");
    expect(alert?.textContent).toContain("router ID 51");
  });

  it("warns that a node's clock is out of step, naming the nodes and the window", () => {
    card({ clockSkewNodes: ["sw-a", "sw-z"], authWindowSeconds: 30 });
    const alert = screen.getByText("A node's clock is out of step").closest("[role=alert]");
    expect(alert?.textContent).toContain("node-a, sw-z are too far from the others.");
    expect(alert?.textContent).toContain("more than 30 s old");
  });

  it("lists each manager with its privileged-services policy and the note that goes with it", () => {
    card({
      managers: [
        { nodeId: "sw-a", hostname: "node-a", state: "allows", localSocket: false },
        { nodeId: "sw-b", hostname: "node-b", state: "refuses", localSocket: false },
        { nodeId: "sw-c", hostname: "node-c", state: "unknown", localSocket: false },
        { nodeId: "sw-d", hostname: "node-d", state: "allows", localSocket: true },
      ],
    });
    const table = screen.getByRole("table", { name: "Managers" });
    const row = (n: string) => within(table).getByText(n).closest("tr") as HTMLElement;
    expect(within(row("node-a")).getByText("Allowed")).toBeTruthy();
    expect(within(row("node-b")).getByText("Refused")).toBeTruthy();
    const setting = within(row("node-b")).getByText(
      "TIERPLAN_AGENT_ALLOW_PRIVILEGED_SERVICES=true",
    );
    expect(setting.className).toContain("font-mono");
    expect(within(row("node-c")).getByText("Unknown")).toBeTruthy();
    expect(
      within(row("node-d")).getByText(/Reached through TierPlan's own Docker socket/),
    ).toBeTruthy();
  });

  it("replaces the managers table with one line when the managers could not be read", () => {
    card({ managersReadable: false, managers: [] });
    expect(screen.queryByRole("table", { name: "Managers" })).toBeNull();
    expect(screen.getByText("TierPlan couldn't read the managers just now.")).toBeTruthy();
  });
});
