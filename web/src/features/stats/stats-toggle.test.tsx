// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { TooltipProvider } from "@/components/ui/tooltip";
import { StatsToggle } from "@/features/stats/stats-toggle";
import "@/i18n";

// The Live stats switch of the services, containers and placement pages (plan 06-33,
// RES-11, D-31, UI-SPEC "Services list").

afterEach(cleanup);

function renderToggle(checked: boolean, onCheckedChange = vi.fn(), hint?: string) {
  render(
    <TooltipProvider>
      <StatsToggle checked={checked} onCheckedChange={onCheckedChange} hint={hint} />
    </TooltipProvider>,
  );
  return onCheckedChange;
}

describe("StatsToggle", () => {
  it("is a switch labelled Live stats, off when the page says so", () => {
    renderToggle(false);
    const sw = screen.getByRole("switch", { name: "Live stats" });
    expect(sw.getAttribute("aria-checked")).toBe("false");
  });

  it("is on when the page says so", () => {
    renderToggle(true);
    expect(screen.getByRole("switch", { name: "Live stats" }).getAttribute("aria-checked")).toBe(
      "true",
    );
  });

  it("asks for the other state when pressed", () => {
    const onChange = renderToggle(false);
    fireEvent.click(screen.getByRole("switch", { name: "Live stats" }));
    expect(onChange).toHaveBeenCalledWith(true);
  });

  it("is turned on by its label too", () => {
    const onChange = renderToggle(false);
    fireEvent.click(screen.getByText("Live stats"));
    expect(onChange).toHaveBeenCalledWith(true);
  });

  it("explains what it adds and that it streams only while the list is open", async () => {
    renderToggle(false);
    act(() => screen.getByRole("switch", { name: "Live stats" }).focus());
    expect(
      (
        await screen.findAllByText(
          "Adds CPU, memory and network columns. Streams only while this list is open.",
        )
      ).length,
    ).toBeGreaterThan(0);
  });

  it("takes other words for a page where it adds something else", async () => {
    renderToggle(false, vi.fn(), "Adds CPU and memory bars to each node.");
    act(() => screen.getByRole("switch", { name: "Live stats" }).focus());
    expect(
      (await screen.findAllByText("Adds CPU and memory bars to each node.")).length,
    ).toBeGreaterThan(0);
  });
});
