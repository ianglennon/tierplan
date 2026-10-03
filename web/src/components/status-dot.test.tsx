// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import type { EnvironmentStatus, HealthState } from "@/api/environments";
import { StatusDot, useHealthLabel } from "@/components/status-dot";
import "@/i18n";

afterEach(cleanup);

function Label({ status }: { status?: EnvironmentStatus }) {
  const label = useHealthLabel();
  return <span data-testid="label">{label(status)}</span>;
}

const dotOf = (container: HTMLElement) => container.querySelector('[aria-hidden="true"]');

// The UI-SPEC "Color" table: dot, text colour and label for every health state.
const TABLE: Array<[HealthState, string, string, RegExp | string]> = [
  ["online", "bg-status-online", "text-status-online-text", "Online"],
  ["degraded", "bg-status-warning", "text-status-warning-text", "Degraded"],
  ["reconnecting", "bg-status-warning", "text-status-warning-text", "Reconnecting…"],
  ["unsupported", "bg-status-warning", "text-status-warning-text", "Unsupported"],
  ["cluster_changed", "bg-status-warning", "text-status-warning-text", "Cluster changed"],
  ["offline", "bg-status-offline", "text-status-offline-text", "Offline"],
  ["revoked", "bg-status-offline", "text-status-offline-text", "Revoked"],
  ["pending", "border-status-unknown", "text-muted-foreground", "Pending"],
  ["unknown", "bg-status-unknown", "text-muted-foreground", "Unknown"],
];

describe("StatusDot", () => {
  it.each(TABLE)("draws %s with its colours and its text label", (state, dot, text, label) => {
    const { container } = render(
      <>
        <StatusDot state={state} label={typeof label === "string" ? label : "x"} />
        <Label status={{ state }} />
      </>,
    );
    const el = dotOf(container);
    expect(el?.className).toContain(dot);
    expect(container.firstElementChild?.className).toContain(text);
    expect(screen.getByTestId("label").textContent).toBe(label);
  });

  it("draws pending as a hollow ring, never a filled dot", () => {
    const { container } = render(<StatusDot state="pending" label="Pending" />);
    const cls = dotOf(container)?.className ?? "";
    expect(cls).toContain("border-2");
    expect(cls).toContain("bg-transparent");
    expect(cls).not.toContain("bg-status-");
  });

  it("pulses reconnecting and stops the pulse under prefers-reduced-motion", () => {
    const { container } = render(<StatusDot state="reconnecting" label="Reconnecting…" />);
    const cls = dotOf(container)?.className ?? "";
    expect(cls).toContain("animate-pulse");
    expect(cls).toContain("motion-reduce:animate-none");
  });

  it("does not pulse any other state", () => {
    for (const [state] of TABLE) {
      if (state === "reconnecting") continue;
      const { container, unmount } = render(<StatusDot state={state} label="x" />);
      expect(dotOf(container)?.className).not.toContain("animate-pulse");
      unmount();
    }
  });

  it("names a label-less dot from the health label so it is never colour alone", () => {
    render(<StatusDot state="pending" />);
    expect(screen.getByRole("img", { name: "Pending" }).className).toContain("border-2");
  });

  it("renders offline with the time it began", () => {
    render(<Label status={{ state: "offline", since: "2026-10-01T14:05:00Z" }} />);
    expect(screen.getByTestId("label").textContent).toMatch(/^Offline since \d{1,2}:\d{2}/);
  });

  it("renders a missing status as Unknown", () => {
    render(<Label />);
    expect(screen.getByTestId("label").textContent).toBe("Unknown");
  });
});
