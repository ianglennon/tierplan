// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { RelativeTime } from "@/components/relative-time";
import { TooltipProvider } from "@/components/ui/tooltip";
import "@/i18n";

function show(value?: string) {
  render(
    <TooltipProvider>
      <RelativeTime value={value} />
    </TooltipProvider>,
  );
}

beforeEach(() => {
  vi.useFakeTimers();
  vi.setSystemTime(new Date("2026-10-01T12:00:00Z"));
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

describe("RelativeTime (D-66)", () => {
  it("renders a <time> with the machine value and the relative text", () => {
    show("2026-10-01T11:55:00Z");
    const el = screen.getByText("5 min ago");
    expect(el.tagName).toBe("TIME");
    expect(el.getAttribute("datetime")).toBe("2026-10-01T11:55:00Z");
  });

  it("is focusable so keyboard users can reach the absolute time", () => {
    show("2026-10-01T11:55:00Z");
    expect(screen.getByText("5 min ago").getAttribute("tabindex")).toBe("0");
  });

  it("shows the absolute local time with the zone in a tooltip on focus", async () => {
    show("2026-10-01T11:55:00Z");
    await act(async () => {
      fireEvent.focus(screen.getByText("5 min ago"));
    });
    const tip = screen.getAllByText(/2026/);
    expect(tip.some((n) => /\d{2}:\d{2}:\d{2}/.test(n.textContent ?? ""))).toBe(true);
  });

  it("moves on every 30 seconds without a reload", () => {
    show("2026-10-01T11:59:40Z");
    expect(screen.getByText("just now")).toBeTruthy();
    act(() => {
      vi.advanceTimersByTime(30_000);
    });
    act(() => {
      vi.advanceTimersByTime(30_000);
    });
    expect(screen.getByText("1 min ago")).toBeTruthy();
  });

  it("shows a dash for a missing or unreadable time", () => {
    show(undefined);
    expect(screen.getByText("—")).toBeTruthy();
    cleanup();
    show("garbage");
    expect(screen.getByText("—")).toBeTruthy();
  });
});
