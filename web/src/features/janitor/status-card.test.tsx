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

  it("says Off, what that means and that no run is scheduled, with a dash for the next run", () => {
    mount(janitorStatus({ enabled: false, nextRunAt: null }));
    const card = screen.getByRole("region", { name: "Janitor status" });
    expect(within(card).getByText("Off")).toBeTruthy();
    expect(within(card).getByText("Nothing is removed until you turn it on.")).toBeTruthy();
    expect(within(card).getByText("—")).toBeTruthy();
    expect(within(card).getByText("Turn the janitor on to schedule runs.")).toBeTruthy();
  });

  it("says a run that waits for a deploy is waiting, and one that works is running", () => {
    const { unmount } = mount(
      janitorStatus({ active: { jobId: "", runId: "r9", state: "waiting" } }),
    );
    expect(screen.getByText("Waiting for a deploy")).toBeTruthy();
    unmount();
    mount(janitorStatus({ active: { jobId: "j9", runId: "r9", state: "running" } }));
    expect(screen.getByText("Running now")).toBeTruthy();
    // The schedule is still the schedule while a run goes.
    expect(screen.getByText("Daily at 03:00, server time (Europe/London).")).toBeTruthy();
  });

  it("says the janitor hasn't run yet when no run has ended", () => {
    mount(janitorStatus());
    const card = screen.getByRole("region", { name: "Janitor status" });
    expect(within(card).getByText("Hasn't run yet")).toBeTruthy();
    expect(within(card).queryByRole("link")).toBeNull();
  });

  it("gives the reason of a run that was skipped, and says nothing needed removing for an empty completed run", () => {
    const { unmount } = mount(
      janitorStatus({ lastRun: runSummary({ state: "skipped", reason: "deploy_in_progress" }) }),
    );
    expect(screen.getByRole("link", { name: "Open run, Skipped" })).toBeTruthy();
    expect(
      screen.getByText(
        "A deploy was in progress. TierPlan will try again at the next scheduled run.",
      ),
    ).toBeTruthy();
    unmount();
    mount(janitorStatus({ lastRun: runSummary() }));
    expect(screen.getByText("Nothing needed removing.")).toBeTruthy();
  });

  it("leaves the space out of the sentence when no size was known, never a zero", () => {
    mount(janitorStatus({ lastRun: runSummary({ counts: removed, estBytes: null }) }));
    expect(screen.getByText("Removed 14 containers, 3 networks, 2 images.")).toBeTruthy();
  });

  it("says a schedule it cannot use leaves nothing scheduled", () => {
    mount(janitorStatus({ nextRunAt: null }));
    expect(screen.getByText("This schedule can't be used, so no run is scheduled.")).toBeTruthy();
  });

  it("shows a skeleton with the heading hidden while the status is on its way", () => {
    mount(undefined, true);
    const card = screen.getByRole("region", { name: "Janitor status" });
    expect(card.getAttribute("aria-busy")).toBe("true");
    expect(within(card).getByText("Loading the janitor status")).toBeTruthy();
    expect(within(card).queryByText("State")).toBeNull();
  });
});
