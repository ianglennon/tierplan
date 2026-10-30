// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { NodeLabelImpact } from "@/api/nodes";
import { LabelImpactPanel } from "@/features/nodes/label-impact-panel";
import "@/i18n";

// The Effect on services panel (plan 06-16, RES-05, D-21, UI-SPEC "Nodes"): what a change of the
// node's labels does to each service's placement constraint, before it is saved.

afterEach(cleanup);

const impact = (extra: Partial<NodeLabelImpact> = {}): NodeLabelImpact => ({
  version: 4,
  rows: [],
  noLongerMatches: 0,
  tasksMayMove: 0,
  added: 0,
  changed: 0,
  removed: 0,
  ...extra,
});

describe("LabelImpactPanel", () => {
  it("is titled Effect on services", () => {
    render(<LabelImpactPanel status="ready" impact={impact()} />);
    expect(screen.getByRole("heading", { name: "Effect on services" })).toBeTruthy();
  });

  it("says Checking services… while the check is pending", () => {
    render(<LabelImpactPanel status="pending" />);
    const status = screen.getByRole("status");
    expect(within(status).getByText("Checking services…")).toBeTruthy();
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("says no service uses the labels when nothing is changed or no constraint names them", () => {
    const { rerender } = render(<LabelImpactPanel status="idle" />);
    expect(
      screen.getByText("No service uses these labels in a placement constraint."),
    ).toBeTruthy();
    rerender(<LabelImpactPanel status="ready" impact={impact()} />);
    expect(
      screen.getByText("No service uses these labels in a placement constraint."),
    ).toBeTruthy();
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("lists each constraint with Service, Constraint, Before and After", () => {
    render(
      <LabelImpactPanel
        status="ready"
        impact={impact({
          rows: [
            {
              serviceId: "s1",
              service: "db",
              constraint: "node.labels.role==db",
              before: true,
              after: false,
            },
            {
              serviceId: "s2",
              service: "cache",
              constraint: "node.labels.disk==ssd",
              before: false,
              after: true,
            },
            {
              serviceId: "s3",
              service: "web",
              constraint: "node.labels.zone==a",
              before: true,
              after: true,
            },
          ],
          noLongerMatches: 1,
          tasksMayMove: 2,
        })}
      />,
    );
    const table = screen.getByRole("table", { name: "Effect on services" });
    expect(
      within(table)
        .getAllByRole("columnheader")
        .map((h) => h.textContent),
    ).toEqual(["Service", "Constraint", "Before", "After"]);
    const rows = within(table).getAllByRole("row").slice(1);
    const cells = rows.map((r) =>
      within(r)
        .getAllByRole("cell")
        .map((c) => c.textContent),
    );
    expect(cells).toEqual([
      ["db", "node.labels.role==db", "Matches", "No longer matches"],
      ["cache", "node.labels.disk==ssd", "Doesn't match", "Now matches"],
      ["web", "node.labels.zone==a", "Matches", "Matches"],
    ]);
  });

  it("says how many tasks the swarm may move, in the singular and the plural", () => {
    const rows = [
      {
        serviceId: "s1",
        service: "db",
        constraint: "node.labels.role==db",
        before: true,
        after: false,
      },
    ];
    const { rerender } = render(
      <LabelImpactPanel
        status="ready"
        impact={impact({ rows, noLongerMatches: 1, tasksMayMove: 2 })}
      />,
    );
    expect(screen.getByText("Swarm may move 2 tasks off this node.")).toBeTruthy();
    rerender(
      <LabelImpactPanel
        status="ready"
        impact={impact({ rows, noLongerMatches: 1, tasksMayMove: 1 })}
      />,
    );
    expect(screen.getByText("Swarm may move 1 task off this node.")).toBeTruthy();
    rerender(
      <LabelImpactPanel
        status="ready"
        impact={impact({ rows, noLongerMatches: 1, tasksMayMove: 0 })}
      />,
    );
    expect(screen.queryByText(/Swarm may move/)).toBeNull();
  });

  it("shows a service name and a constraint as text, never as markup", () => {
    const { container } = render(
      <LabelImpactPanel
        status="ready"
        impact={impact({
          rows: [
            {
              serviceId: "s1",
              service: "<b>db</b>",
              constraint: "node.labels.x==<i>1</i>",
              before: true,
              after: false,
            },
          ],
          noLongerMatches: 1,
        })}
      />,
    );
    expect(screen.getByText("<b>db</b>")).toBeTruthy();
    expect(container.querySelector("b, i")).toBeNull();
  });

  it("words a failed check and offers to retry it", () => {
    const onRetry = vi.fn();
    render(<LabelImpactPanel status="error" onRetry={onRetry} />);
    expect(screen.getByRole("alert").textContent).toContain(
      "Couldn't check which services use these labels.",
    );
    fireEvent.click(screen.getByRole("button", { name: "Retry checking services" }));
    expect(onRetry).toHaveBeenCalledTimes(1);
  });
});
