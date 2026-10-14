// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { node, service } from "@/features/placement/fixtures";
import { PlacementRulesCard } from "@/features/placement/placement-rules-card";
import "@/i18n";

// The Placement rules card of a service's Placement tab (plan 06-12, RES-12, D-19, UI-SPEC
// "Placement").

afterEach(cleanup);

const NODES = [
  node({ id: "n1", hostname: "mgr-1", role: "manager", labels: { disk: "ssd" } }),
  node({ id: "n2", hostname: "wrk-1", role: "worker", labels: { disk: "ssd" } }),
  node({ id: "n3", hostname: "wrk-2", role: "worker", labels: {} }),
  node({ id: "n4", hostname: "wrk-3", role: "worker", labels: { disk: "hdd" } }),
];

const card = () => screen.getByRole("region", { name: "Placement rules" });

describe("PlacementRulesCard", () => {
  it("lists each constraint in mono with how many of the nodes it matches", () => {
    render(
      <PlacementRulesCard
        service={service({ constraints: ["node.role==manager", "node.labels.disk==ssd"] })}
        nodes={NODES}
      />,
    );
    const first = screen.getByText("node.role==manager");
    expect(first.className).toContain("font-mono");
    expect(within(card()).getByText("Matches 1 of 4 nodes")).toBeTruthy();
    expect(screen.getByText("node.labels.disk==ssd")).toBeTruthy();
    expect(within(card()).getByText("Matches 2 of 4 nodes")).toBeTruthy();
  });

  it("warns, with an icon and words, of a constraint that matches no node", () => {
    const { container } = render(
      <PlacementRulesCard
        service={service({ constraints: ["node.labels.gpu==true"] })}
        nodes={NODES}
      />,
    );
    const warning = within(card()).getByText("Matches no nodes");
    expect(warning.parentElement?.querySelector("svg.lucide-triangle-alert")).toBeTruthy();
    expect(container.textContent).not.toContain("Matches 0 of");
  });

  it("says a constraint it cannot read is unreadable, not that it matches nothing", () => {
    render(
      <PlacementRulesCard service={service({ constraints: ["node.role=worker"] })} nodes={NODES} />,
    );
    expect(within(card()).getByText("node.role=worker")).toBeTruthy();
    expect(within(card()).getByText("Bayplan can't read this constraint.")).toBeTruthy();
    expect(within(card()).queryByText(/Matches/)).toBeNull();
  });

  it("lists the spread preferences and the limit per node", () => {
    render(
      <PlacementRulesCard
        service={service({
          constraints: ["node.role==worker"],
          preferences: ["node.labels.zone", "engine.labels.rack"],
          maxReplicas: 2,
        })}
        nodes={NODES}
      />,
    );
    expect(within(card()).getByText("Spread over node.labels.zone")).toBeTruthy();
    expect(within(card()).getByText("Spread over engine.labels.rack")).toBeTruthy();
    expect(within(card()).getByText("Max replicas per node: 2")).toBeTruthy();
    expect(within(card()).getByText("Matches 3 of 4 nodes")).toBeTruthy();
  });

  it("leaves out the limit when the spec sets none", () => {
    render(
      <PlacementRulesCard
        service={service({ constraints: ["node.role==worker"] })}
        nodes={NODES}
      />,
    );
    expect(within(card()).queryByText(/Max replicas/)).toBeNull();
  });

  it("says the tasks can run anywhere when there are no rules", () => {
    render(<PlacementRulesCard service={service()} nodes={NODES} />);
    expect(within(card()).getByText("No placement rules. Tasks can run on any node.")).toBeTruthy();
  });

  it("counts a one-node swarm in the singular", () => {
    render(
      <PlacementRulesCard
        service={service({ constraints: ["node.role==manager"] })}
        nodes={[NODES[0] as ReturnType<typeof node>]}
      />,
    );
    expect(within(card()).getByText("Matches 1 of 1 node")).toBeTruthy();
  });

  it("draws a hostile constraint and label as text, never as markup", () => {
    const hostile = "node.labels.x==<img src=x onerror=alert(1)>";
    const { container } = render(
      <PlacementRulesCard
        service={service({ constraints: [hostile], preferences: ["<script>x</script>"] })}
        nodes={NODES}
      />,
    );
    expect(container.querySelector("img, script")).toBeNull();
    expect(container.textContent).toContain("<script>x</script>");
  });
});
