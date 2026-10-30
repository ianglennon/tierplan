// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { CertExpiry } from "@/components/cert-expiry";
import { TooltipProvider } from "@/components/ui/tooltip";
import { formatAbsolute } from "@/lib/format";
import "@/i18n";

const NOW = new Date("2026-10-03T12:00:00Z");
const HOUR = 3_600_000;

function ahead(ms: number): string {
  return new Date(NOW.getTime() + ms).toISOString();
}

function show(notAfter: string, renewalFailing?: boolean) {
  render(
    <TooltipProvider>
      <CertExpiry notAfter={notAfter} renewalFailing={renewalFailing} />
    </TooltipProvider>,
  );
}

beforeEach(() => {
  vi.useFakeTimers();
  vi.setSystemTime(NOW);
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

const warning = (el: HTMLElement) => el.classList.contains("text-status-warning-text");
const offline = (el: HTMLElement) => el.classList.contains("text-status-offline-text");

describe("CertExpiry", () => {
  it("counts whole days down, so 6 days 23 hours is 6 days, in the warning colour", () => {
    show(ahead(6 * 24 * HOUR + 23 * HOUR));
    const text = screen.getByText("Expires in 6 days");
    expect(warning(text)).toBe(true);
    expect(offline(text)).toBe(false);
  });

  it("adds Not renewing when renewal is failing", () => {
    show(ahead(3 * 24 * HOUR), true);
    expect(screen.getByText("Expires in 3 days")).toBeTruthy();
    expect(screen.getByText("Not renewing")).toBeTruthy();
  });

  it("does not say Not renewing when it is not", () => {
    show(ahead(3 * 24 * HOUR), false);
    expect(screen.queryByText("Not renewing")).toBeNull();
  });

  it("keeps the neutral colour at 30 days and at exactly 7 days", () => {
    show(ahead(30 * 24 * HOUR));
    const thirty = screen.getByText("Expires in 30 days");
    expect(warning(thirty) || offline(thirty)).toBe(false);
    cleanup();
    show(ahead(7 * 24 * HOUR));
    const seven = screen.getByText("Expires in 7 days");
    expect(warning(seven) || offline(seven)).toBe(false);
  });

  it("reads one day as 1 day", () => {
    show(ahead(36 * HOUR));
    expect(screen.getByText("Expires in 1 day")).toBeTruthy();
  });

  it("says 0 days under a day, never a negative or blank count", () => {
    show(ahead(2 * HOUR));
    const text = screen.getByText("Expires in 0 days");
    expect(warning(text)).toBe(true);
  });

  it("says Expired with the relative time, in the offline colour, after it ran out", () => {
    show(ahead(-2 * HOUR));
    const text = screen.getByText("Expired 2 h ago");
    expect(offline(text)).toBe(true);
    expect(warning(text)).toBe(false);
    expect(screen.queryByText(/Expires in/)).toBeNull();
  });

  it("is expired at the very moment it runs out", () => {
    show(ahead(0));
    expect(screen.getByText("Expired just now")).toBeTruthy();
  });

  it("shows the absolute local date, time and zone in a tooltip on focus", async () => {
    const notAfter = ahead(40 * 24 * HOUR);
    show(notAfter);
    const trigger = screen.getByText("Expires in 40 days");
    expect(trigger.getAttribute("tabindex")).toBe("0");
    await act(async () => {
      fireEvent.focus(trigger);
    });
    const tips = screen.getAllByText(formatAbsolute(notAfter));
    expect(tips.length).toBeGreaterThan(0);
  });

  it("moves with the shared clock without a reload", () => {
    show(ahead(7 * 24 * HOUR + 10_000));
    expect(screen.getByText("Expires in 7 days")).toBeTruthy();
    act(() => {
      vi.advanceTimersByTime(30_000);
    });
    expect(screen.getByText("Expires in 6 days")).toBeTruthy();
  });
});
