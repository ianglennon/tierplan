// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { TooltipProvider } from "@/components/ui/tooltip";
import { RestartLoopBadge } from "@/features/restart-loop/restart-loop-badge";
import "@/i18n";

// The Restart loop badge (plan 06-34, RES-13, D-15, UI-SPEC "Restart loop" and Color
// "Restart-loop badge treatment").

afterEach(cleanup);

const LOOP = {
  state: "flagged" as const,
  count: 3,
  windowMinutes: 10,
  latestError: "task: non-zero exit (1)",
  since: "2026-10-14T09:00:00Z",
};

function mount(loop = LOOP) {
  return render(
    <TooltipProvider>
      <RestartLoopBadge loop={loop} />
    </TooltipProvider>,
  );
}

describe("RestartLoopBadge", () => {
  it("is an outline badge reading Restart loop, never a red fill", () => {
    mount();
    const badge = screen.getByText("Restart loop").closest("[data-variant]");
    expect(badge?.getAttribute("data-variant")).toBe("outline");
    expect(badge?.className ?? "").not.toMatch(/bg-(destructive|status-offline|red)/);
  });

  it("carries the offline dot and the offline text colour", () => {
    mount();
    const text = screen.getByText("Restart loop").closest("span.text-status-offline-text");
    expect(text).not.toBeNull();
    expect(text?.querySelector(".bg-status-offline")).not.toBeNull();
  });

  it("says how many tasks failed in the window and the latest error in a tooltip", async () => {
    mount();
    fireEvent.focus(screen.getByText("Restart loop"));
    const tip = await screen.findByRole("tooltip");
    expect(tip.textContent).toBe(
      "3 failed tasks in the last 10 minutes. Latest: task: non-zero exit (1)",
    );
  });

  it("reads as singular for one task and one minute", async () => {
    mount({ ...LOOP, count: 1, windowMinutes: 1 });
    fireEvent.focus(screen.getByText("Restart loop"));
    const tip = await screen.findByRole("tooltip");
    expect(tip.textContent).toContain("1 failed task in the last 1 minute.");
  });

  it("gives the count alone when no error is known", async () => {
    mount({ ...LOOP, latestError: "" });
    fireEvent.focus(screen.getByText("Restart loop"));
    const tip = await screen.findByRole("tooltip");
    expect(tip.textContent).toBe("3 failed tasks in the last 10 minutes.");
  });

  it("clamps a long error to three lines and draws it as text, never as markup", async () => {
    mount({ ...LOOP, latestError: "<b>boom</b> ".repeat(60) });
    fireEvent.focus(screen.getByText("Restart loop"));
    await screen.findByRole("tooltip");
    expect(document.querySelector(".line-clamp-3")?.textContent).toContain("<b>boom</b>");
    expect(document.querySelector("b")).toBeNull();
  });
});

// The muted variant (plan 06-35, RES-13, D-16): a neutral outline badge with BellOff and Muted.
describe("RestartLoopBadge muted", () => {
  const MUTED = { ...LOOP, state: "muted" as const };

  it("is a neutral outline badge with BellOff reading Muted, and no offline dot", () => {
    mount(MUTED);
    const badge = screen.getByText("Muted").closest("[data-variant]");
    expect(badge?.getAttribute("data-variant")).toBe("outline");
    expect(badge?.querySelector(".lucide-bell-off")).not.toBeNull();
    expect(badge?.querySelector(".bg-status-offline")).toBeNull();
    expect(badge?.className ?? "").not.toMatch(/status-offline/);
    expect(screen.queryByText("Restart loop")).toBeNull();
  });

  it("says in a tooltip that the alerts are muted and where to unmute them", async () => {
    mount(MUTED);
    fireEvent.focus(screen.getByText("Muted"));
    const tip = await screen.findByRole("tooltip");
    expect(tip.textContent).toBe(
      "Restart-loop alerts are muted for this service. Unmute them from the More menu.",
    );
  });

  it("shows for a muted service that is not looping, with no loop at all", () => {
    render(
      <TooltipProvider>
        <RestartLoopBadge muted />
      </TooltipProvider>,
    );
    expect(screen.getByText("Muted")).toBeTruthy();
  });

  it("shows nothing without a loop and without a mute", () => {
    const { container } = render(
      <TooltipProvider>
        <RestartLoopBadge />
      </TooltipProvider>,
    );
    expect(container.textContent).toBe("");
  });
});
