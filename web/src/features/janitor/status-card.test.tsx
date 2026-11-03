// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import type { JanitorStatus } from "@/api/janitor";
import { TooltipProvider } from "@/components/ui/tooltip";
import { StatusCard } from "@/features/janitor/status-card";
import { formatServerTime } from "@/features/janitor/status-text";
import "@/i18n";
import { janitorStatus, runSummary } from "@/test-janitor";
import { TestRouter } from "@/test-router";

afterEach(cleanup);

function mount(status: JanitorStatus | undefined, loading = false) {
  return render(
    <TestRouter>
      <TooltipProvider>
        <StatusCard envId="e1" status={status} loading={loading} />
      </TooltipProvider>
    </TestRouter>,
  );
}

const removed = {
  failed: 0,
  kept: 0,
  removedContainers: 14,
  removedImages: 2,
  removedNetworks: 3,
  skippedOffline: 0,
};

describe("StatusCard", () => {
  it("says the janitor is on, what the last run removed and when the next one is", () => {
    const status = janitorStatus({
      lastRun: runSummary({ counts: removed, estBytes: 1_200_000_000 }),
    });
    mount(status);

    const card = screen.getByRole("region", { name: "Janitor status" });
    expect(within(card).getByText("State")).toBeTruthy();
    expect(within(card).getByText("On")).toBeTruthy();
    expect(within(card).getByText("Daily at 03:00, server time (Europe/London).")).toBeTruthy();

    // The last run: outcome icon and word as a link to the run, with what it removed.
    const link = within(card).getByRole("link", { name: "Open run, Completed" });
    expect(link.getAttribute("href")).toBe("/env/e1/janitor/runs/r1");
    expect(
      within(card).getByText("Removed 14 containers, 3 networks, 2 images, ~1.2 GB."),
    ).toBeTruthy();

    // The next run: the server's wall clock, and how far away it is.
    expect(within(card).getByText("Next run")).toBeTruthy();
    expect(
      within(card).getByText(formatServerTime(status.nextRunAt as string, "Europe/London")),
    ).toBeTruthy();
    expect(within(card).getByText("in 5 hours")).toBeTruthy();
  });
});
