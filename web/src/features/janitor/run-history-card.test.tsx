// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { JanitorRunSummary, JanitorStatus } from "@/api/janitor";
import { TooltipProvider } from "@/components/ui/tooltip";
import { RunHistoryCard } from "@/features/janitor/run-history-card";
import { formatStarted } from "@/features/janitor/status-text";
import "@/i18n";
import { janitorStatus, runSummary } from "@/test-janitor";
import { TestRouter } from "@/test-router";

let runs: JanitorRunSummary[];
let client: QueryClient;
let asked: string[];

beforeEach(() => {
  setCsrfToken(null);
  runs = [];
  asked = [];
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      if (url.pathname === "/api/env/e1/janitor/runs") {
        asked.push(url.searchParams.get("limit") ?? "");
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
});
