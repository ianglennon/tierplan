// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { NeedsTooltip } from "@/components/needs-tooltip";
import { Button } from "@/components/ui/button";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import "@/i18n";

afterEach(cleanup);

function renderIt(reason: string | undefined, onClick = vi.fn()) {
  render(
    <TooltipProvider>
      <NeedsTooltip reason={reason}>
        <Button type="button" onClick={onClick}>
          Delete user
        </Button>
      </NeedsTooltip>
      <Toaster />
    </TooltipProvider>,
  );
  return onClick;
}

describe("NeedsTooltip", () => {
  it("marks the control aria-disabled, not disabled, and keeps it in the tab order", () => {
    renderIt("Needs Admin");
    const button = screen.getByRole("button", { name: "Delete user" });
    expect(button.getAttribute("aria-disabled")).toBe("true");
    expect(button.hasAttribute("disabled")).toBe(false);
    expect(button.getAttribute("tabindex")).not.toBe("-1");
    button.focus();
    expect(document.activeElement).toBe(button);
  });

  it("links the reason to the control with aria-describedby", () => {
    renderIt("Needs Admin");
    const button = screen.getByRole("button", { name: "Delete user" });
    const id = button.getAttribute("aria-describedby") ?? "";
    expect(id).not.toBe("");
    expect(document.getElementById(id)?.textContent).toBe("Needs Admin");
  });

  it("shows the reason in a tooltip on keyboard focus and on hover", async () => {
    renderIt("Needs Admin");
    const button = screen.getByRole("button", { name: "Delete user" });
    fireEvent.focus(button);
    expect((await screen.findAllByText("Needs Admin")).length).toBeGreaterThan(0);
    expect(await screen.findByRole("tooltip")).toBeTruthy();
    fireEvent.blur(button);
    await waitFor(() => expect(screen.queryByRole("tooltip")).toBeNull());
    fireEvent.pointerMove(button);
    expect(await screen.findByRole("tooltip")).toBeTruthy();
  });

  it("ignores a click and shows the reason as a toast instead", async () => {
    const onClick = renderIt("Only an instance admin can do this");
    fireEvent.click(screen.getByRole("button", { name: "Delete user" }));
    expect(onClick).not.toHaveBeenCalled();
    await waitFor(() =>
      expect(
        screen
          .getAllByText("Only an instance admin can do this")
          .some((el) => el.getAttribute("role") === "status"),
      ).toBe(true),
    );
  });

  it("also ignores the keyboard, which a button turns into a click", () => {
    const onClick = renderIt("Needs Admin");
    const button = screen.getByRole("button", { name: "Delete user" });
    button.focus();
    fireEvent.keyDown(button, { key: "Enter" });
    fireEvent.click(button);
    expect(onClick).not.toHaveBeenCalled();
  });

  it("leaves the control alone when nothing is needed", () => {
    const onClick = renderIt(undefined);
    const button = screen.getByRole("button", { name: "Delete user" });
    expect(button.getAttribute("aria-disabled")).toBeNull();
    expect(button.getAttribute("aria-describedby")).toBeNull();
    fireEvent.click(button);
    expect(onClick).toHaveBeenCalledTimes(1);
  });
});
