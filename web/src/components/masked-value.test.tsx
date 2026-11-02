// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { MaskedValue } from "@/components/masked-value";
import { TooltipProvider } from "@/components/ui/tooltip";
import "@/i18n";

// MaskedValue (plan 03-22, D-15a, UI-SPEC "Component Inventory").

afterEach(cleanup);

const DOTS = "••••••••";

describe("MaskedValue", () => {
  it("shows eight dots with a tooltip saying why, and no toggle and no value for someone who cannot reveal", async () => {
    const { container } = render(
      <TooltipProvider>
        <MaskedValue />
      </TooltipProvider>,
    );
    const dots = screen.getByText(DOTS);
    expect(screen.queryByRole("button")).toBeNull();
    expect(container.textContent).toBe(DOTS);
    fireEvent.focus(dots);
    expect(await screen.findByRole("tooltip")).toBeTruthy();
    expect(screen.getByRole("tooltip").textContent).toContain("secrets.reveal");
  });

  it("gives a holder of secrets.reveal a Show value toggle that reveals the value and hides it again", async () => {
    render(
      <TooltipProvider>
        <MaskedValue canReveal value="hunter2" />
      </TooltipProvider>,
    );
    // The value is not on the page until it is asked for.
    expect(screen.queryByText("hunter2")).toBeNull();
    expect(screen.getByText(DOTS)).toBeTruthy();
    const toggle = screen.getByRole("button", { name: "Show value" });
    expect(toggle.getAttribute("aria-pressed")).toBe("false");

    fireEvent.click(toggle);
    expect(toggle.getAttribute("aria-pressed")).toBe("true");
    expect(screen.getByText("hunter2")).toBeTruthy();
    expect(screen.queryByText(DOTS)).toBeNull();

    fireEvent.click(toggle);
    expect(toggle.getAttribute("aria-pressed")).toBe("false");
    await waitFor(() => expect(screen.queryByText("hunter2")).toBeNull());
    expect(screen.getByText(DOTS)).toBeTruthy();
  });

  it("names the variable on the toggle when it is given one, so a table of them can be told apart", () => {
    render(
      <TooltipProvider>
        <MaskedValue canReveal value="x" name="DB_PASSWORD" />
      </TooltipProvider>,
    );
    expect(screen.getByRole("button", { name: "Show value of DB_PASSWORD" })).toBeTruthy();
  });

  it("wraps a long revealed value instead of widening its column", () => {
    render(
      <TooltipProvider>
        <MaskedValue canReveal value={"v".repeat(300)} />
      </TooltipProvider>,
    );
    fireEvent.click(screen.getByRole("button", { name: "Show value" }));
    expect(screen.getByText("v".repeat(300)).className).toContain("break-all");
  });
});
