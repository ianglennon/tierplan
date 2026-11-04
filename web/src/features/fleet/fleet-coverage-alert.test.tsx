// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import type { FleetNotIncluded } from "@/api/fleet";
import { FleetCoverageAlert } from "@/features/fleet/fleet-coverage-alert";
import "@/i18n";

afterEach(cleanup);

function missing(
  name: string,
  reason: FleetNotIncluded["reason"] = "offline",
  nodes = 0,
): FleetNotIncluded {
  return { envId: `id-${name}`, envName: name, reason, nodes };
}

describe("FleetCoverageAlert", () => {
  it("names an environment that is offline and says it will appear when it reports again", () => {
    render(<FleetCoverageAlert notIncluded={[missing("prod")]} />);
    const alert = screen.getByRole("status");
    expect(alert.textContent).toContain("Some environments aren't included");
    expect(alert.textContent).toContain(
      "prod (offline). They will appear here when they report again.",
    );
  });

  it("says nothing when every environment was read", () => {
    const { container } = render(<FleetCoverageAlert notIncluded={[]} />);
    expect(container.textContent).toBe("");
    cleanup();
    const again = render(<FleetCoverageAlert notIncluded={null} />);
    expect(again.container.textContent).toBe("");
  });

  it("gives each reason its own words, and a node count for an environment read in part", () => {
    render(
      <FleetCoverageAlert
        notIncluded={[
          missing("a", "not_reporting"),
          missing("b", "waiting_first_agent"),
          missing("c", "nodes_not_reporting", 2),
          missing("d", "nodes_not_reporting", 1),
        ]}
      />,
    );
    const text = screen.getByRole("status").textContent ?? "";
    expect(text).toContain("a (not reporting)");
    expect(text).toContain("b (waiting for the first agent)");
    expect(text).toContain("c (2 nodes not reporting)");
    expect(text).toContain("d (1 node not reporting)");
  });

  it("names five and folds the rest behind Show 2 more", () => {
    const names = ["e1", "e2", "e3", "e4", "e5", "e6", "e7"];
    render(<FleetCoverageAlert notIncluded={names.map((n) => missing(n))} />);
    const alert = screen.getByRole("status");
    for (const n of names.slice(0, 5)) expect(alert.textContent).toContain(`${n} (offline)`);
    expect(screen.queryByText("e6 (offline)")).toBeNull();
    const more = screen.getByRole("button", { name: "Show 2 more" });
    fireEvent.click(more);
    expect(screen.getByText("e6 (offline)")).toBeTruthy();
    expect(screen.getByText("e7 (offline)")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Show fewer" })).toBeTruthy();
  });

  it("does not offer Show more for five or fewer", () => {
    render(<FleetCoverageAlert notIncluded={["a", "b", "c", "d", "e"].map((n) => missing(n))} />);
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("draws a name as text, never as markup", () => {
    render(<FleetCoverageAlert notIncluded={[missing("<img src=x onerror=alert(1)>")]} />);
    const alert = screen.getByRole("status");
    expect(alert.textContent).toContain("<img src=x onerror=alert(1)> (offline)");
    expect(alert.querySelector("img")).toBeNull();
  });
});
