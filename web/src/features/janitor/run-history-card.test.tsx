// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { useState } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { JanitorRunSummary, JanitorStatus } from "@/api/janitor";
import { TooltipProvider } from "@/components/ui/tooltip";
import { RunHistoryCard } from "@/features/janitor/run-history-card";
import { formatServerTime, formatStarted } from "@/features/janitor/status-text";
import "@/i18n";
import { janitorStatus, runSummary } from "@/test-janitor";
import { TestRouter } from "@/test-router";

let runs: JanitorRunSummary[];
let client: QueryClient;
let asked: string[];
let failing: boolean;

beforeEach(() => {
  setCsrfToken(null);
  runs = [];
  asked = [];
  failing = false;
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      if (url.pathname === "/api/env/e1/janitor/runs") {
        asked.push(url.searchParams.get("limit") ?? "");
        if (failing) return Response.json({ title: "x", status: 500 }, { status: 500 });
        const limit = Number(url.searchParams.get("limit") ?? 25);
        return Response.json({ runs: runs.slice(0, limit), total: runs.length });
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  client.clear();
});

function mount(status: JanitorStatus | undefined = janitorStatus(), limit = 25) {
  return render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <RunHistoryCard envId="e1" limit={limit} onLimitChange={() => {}} status={status} />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

describe("RunHistoryCard", () => {
  it("lists the runs newest first with when, what started them, how they ended, what they removed and freed", async () => {
    const scheduled = runSummary({
      id: "r2",
      trigger: "schedule",
      actor: "system:janitor",
      actorName: "TierPlan",
      startedAt: "2026-11-03T03:04:00Z",
      finishedAt: "2026-11-03T03:04:30Z",
      durationSeconds: 30,
      counts: {
        failed: 0,
        kept: 0,
        removedContainers: 14,
        removedImages: 2,
        removedNetworks: 3,
        skippedOffline: 0,
      },
      estBytes: 1_200_000_000,
    });
    const manual = runSummary({
      id: "r1",
      startedAt: "2026-11-02T10:00:00Z",
      finishedAt: "2026-11-02T10:00:12Z",
      durationSeconds: 12,
    });
    runs = [scheduled, manual];
    mount();

    const table = await screen.findByRole("table", { name: "Run history" });
    const rows = within(table).getAllByRole("row");
    // The header, then the runs in the order the server gave them.
    expect(rows).toHaveLength(3);
    const first = within(rows[1] as HTMLElement);
    expect(first.getByText("Scheduled")).toBeTruthy();
    expect(first.getByText("Completed")).toBeTruthy();
    expect(first.getByText("14 containers, 3 networks, 2 images")).toBeTruthy();
    expect(first.getByText("~1.2 GB")).toBeTruthy();
    expect(first.getByText("30 seconds")).toBeTruthy();
    const second = within(rows[2] as HTMLElement);
    expect(second.getByText("Clean now")).toBeTruthy();
    expect(second.getByText("by Ada")).toBeTruthy();
    expect(second.getByText("Nothing removed")).toBeTruthy();
    // No size was known.
    expect(second.getAllByText("—").length).toBeGreaterThan(0);

    // Each row's one link is its Started cell and opens the run.
    const link = first.getByRole("link", {
      name: `Open run from ${formatStarted("2026-11-03T03:04:00Z")}`,
    });
    expect(link.getAttribute("href")).toBe("/env/e1/janitor/runs/r2");
    expect(within(rows[1] as HTMLElement).getAllByRole("link")).toHaveLength(1);

    // The caption states the retention the status gave.
    expect(
      screen.getByText(
        "Every run, with what it removed and why. Kept for 90 days or the last 100 runs, whichever keeps more.",
      ),
    ).toBeTruthy();
    expect(screen.getByText("2 runs")).toBeTruthy();
  });

  it("says No runs yet and when the first scheduled run is", async () => {
    const status = janitorStatus();
    mount(status);
    expect(await screen.findByText("No runs yet")).toBeTruthy();
    const when = formatServerTime(status.nextRunAt as string, "Europe/London");
    expect(
      screen.getByText(
        `The first scheduled run is ${when}. To see what it would remove right now, choose Clean now.`,
      ),
    ).toBeTruthy();
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("says no run is scheduled when the janitor is off", async () => {
    mount(janitorStatus({ enabled: false, nextRunAt: null }));
    expect(await screen.findByText("No runs yet")).toBeTruthy();
    expect(
      screen.getByText(
        "The janitor is off, so no run is scheduled. To see what it would remove right now, choose Clean now.",
      ),
    ).toBeTruthy();
  });

  it("gives the reason of a skipped run in the Removed cell and a dash when no size was known", async () => {
    runs = [runSummary({ state: "skipped", reason: "deploy_in_progress", estBytes: null })];
    mount();
    const row = (await screen.findAllByRole("row"))[1] as HTMLElement;
    expect(
      within(row).getByText(
        "A deploy was in progress. TierPlan will try again at the next scheduled run.",
      ),
    ).toBeTruthy();
    expect(within(row).getByText("Skipped")).toBeTruthy();
    // Space freed is a dash, not 0.
    const cells = within(row).getAllByRole("cell");
    expect(cells[4]?.textContent).toBe("—");
  });

  it("labels a run the job hub refused, which never began, and shows a running run without a duration", async () => {
    runs = [
      runSummary({
        id: "r3",
        state: "failed",
        reason: "failed",
        jobId: "",
        durationSeconds: 0,
      }),
      runSummary({
        id: "r2",
        state: "running",
        finishedAt: null,
        durationSeconds: null,
      }),
    ];
    mount();
    const rows = await screen.findAllByRole("row");
    expect(within(rows[1] as HTMLElement).getByText("Didn't start")).toBeTruthy();
    const running = within(rows[2] as HTMLElement);
    expect(running.getByText("Running")).toBeTruthy();
    expect(within(rows[2] as HTMLElement).getAllByRole("cell")[5]?.textContent).toBe("—");
  });

  it("shows 25 of 30 runs, asks for 50 with Show 25 more, and then has no button", async () => {
    runs = Array.from({ length: 30 }, (_, i) =>
      runSummary({
        id: `r${i}`,
        startedAt: new Date(Date.UTC(2026, 10, 1, 0, 30 - i)).toISOString(),
      }),
    );
    function Harness() {
      const [limit, setLimit] = useState(25);
      return (
        <QueryClientProvider client={client}>
          <TestRouter>
            <TooltipProvider>
              <RunHistoryCard
                envId="e1"
                limit={limit}
                onLimitChange={setLimit}
                status={janitorStatus()}
              />
            </TooltipProvider>
          </TestRouter>
        </QueryClientProvider>
      );
    }
    render(<Harness />);
    expect(await screen.findAllByRole("link", { name: /^Open run from / })).toHaveLength(25);
    expect(screen.getByText("25 runs")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Show 25 more" }));
    await waitFor(() =>
      expect(screen.getAllByRole("link", { name: /^Open run from / })).toHaveLength(30),
    );
    expect(asked).toContain("50");
    expect(screen.getByText("30 runs")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Show 25 more" })).toBeNull();
  });

  it("offers no more at 100 runs, however many exist", async () => {
    runs = Array.from({ length: 120 }, (_, i) => runSummary({ id: `r${i}` }));
    mount(janitorStatus(), 100);
    expect(await screen.findAllByRole("link", { name: /^Open run from / })).toHaveLength(100);
    expect(screen.queryByRole("button", { name: "Show 25 more" })).toBeNull();
  });

  it("draws the title at once and six skeleton rows until the first answer", () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(() => new Promise<Response>(() => {})),
    );
    mount();
    expect(screen.getByText("Run history")).toBeTruthy();
    const loading = screen.getByRole("status", { name: "Loading the run history" });
    expect(loading.children).toHaveLength(6);
    expect(
      screen.getByText(
        "Every run, with what it removed and why. Kept for 90 days or the last 100 runs, whichever keeps more.",
      ),
    ).toBeTruthy();
  });

  it("says it couldn't load the history and reads it again on request", async () => {
    failing = true;
    mount();
    expect(await screen.findByText("Couldn't load the janitor.")).toBeTruthy();
    failing = false;
    runs = [runSummary()];
    fireEvent.click(screen.getByRole("button", { name: "Retry loading janitor" }));
    expect(await screen.findByRole("table", { name: "Run history" })).toBeTruthy();
  });
});
