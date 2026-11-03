// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { JanitorStatus } from "@/api/janitor";
import { TooltipProvider } from "@/components/ui/tooltip";
import { JanitorCard } from "@/features/janitor/janitor-card";
import { formatServerTime } from "@/features/janitor/status-text";
import "@/i18n";
import { janitorStatus, runSummary } from "@/test-janitor";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

let me: ReturnType<typeof makeMe>;
let client: QueryClient;
let answer: () => Response | Promise<Response>;

beforeEach(() => {
  setCsrfToken(null);
  me = makeMe({ envs: { e1: "viewer" } });
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  answer = () => Response.json(janitorStatus());
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(me);
      if (path === "/api/env/e1/janitor") return answer();
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  client.clear();
});

function mount() {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <JanitorCard envId="e1" />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
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

const answerWith = (status: JanitorStatus) => {
  answer = () => Response.json(status);
};

describe("JanitorCard", () => {
  it("says the janitor is on, how the last run ended and what it removed, and links to the page", async () => {
    answerWith(janitorStatus({ lastRun: runSummary({ counts: removed }) }));
    mount();
    const card = await screen.findByRole("region", { name: "Janitor" });
    await within(card).findByText("On, Daily at 03:00");
    expect(within(card).getByText("Completed")).toBeTruthy();
    expect(within(card).getByText("Removed 14 containers, 3 networks, 2 images.")).toBeTruthy();
    expect(within(card).getByRole("link", { name: "Open janitor" }).getAttribute("href")).toBe(
      "/env/e1/janitor",
    );
    // A clean run has no problem line.
    expect(within(card).queryByText(/failed in the last run/)).toBeNull();
  });

  it("adds a warning line, linked to the run, when items failed or a node wasn't reporting", async () => {
    answerWith(
      janitorStatus({
        lastRun: runSummary({
          id: "r7",
          state: "partial",
          counts: { ...removed, failed: 2, skippedOffline: 1 },
        }),
      }),
    );
    mount();
    const card = await screen.findByRole("region", { name: "Janitor" });
    const failed = await within(card).findByRole("link", {
      name: "2 items failed in the last run.",
    });
    expect(failed.getAttribute("href")).toBe("/env/e1/janitor/runs/r7");
    const offline = within(card).getByRole("link", {
      name: "1 item was skipped: its node wasn't reporting.",
    });
    expect(offline.getAttribute("href")).toBe("/env/e1/janitor/runs/r7");
    expect(within(card).getByText("Partly done")).toBeTruthy();
  });

  it("says it hasn't run yet and when the first run is", async () => {
    const status = janitorStatus();
    answerWith(status);
    mount();
    const card = await screen.findByRole("region", { name: "Janitor" });
    expect(await within(card).findByText("Hasn't run yet")).toBeTruthy();
    expect(
      within(card).getByText(
        `First run ${formatServerTime(status.nextRunAt as string, "Europe/London")}.`,
      ),
    ).toBeTruthy();
  });

  it("says Off when the schedule is off, and Running now while a run goes", async () => {
    answerWith(janitorStatus({ enabled: false, nextRunAt: null }));
    mount();
    expect(await screen.findByText("Off")).toBeTruthy();
    expect(screen.queryByText(/First run/)).toBeNull();
    cleanup();
    client.clear();
    answerWith(janitorStatus({ active: { jobId: "j1", runId: "r9", state: "running" } }));
    mount();
    expect(await screen.findByText("Running now")).toBeTruthy();
  });

  it("says it couldn't read the janitor and still offers the page", async () => {
    answer = () => Response.json({ title: "x", status: 500 }, { status: 500 });
    mount();
    const card = await screen.findByRole("region", { name: "Janitor" });
    expect(await within(card).findByText("Couldn't read the janitor.")).toBeTruthy();
    expect(within(card).getByRole("link", { name: "Open janitor" })).toBeTruthy();
  });

  it("shows a skeleton while the status is on its way", async () => {
    answer = () => new Promise<Response>(() => {});
    mount();
    const card = await screen.findByRole("region", { name: "Janitor" });
    expect(card.getAttribute("aria-busy")).toBe("true");
    expect(within(card).getByText("Loading the janitor")).toBeTruthy();
  });

  it("is hidden for a user who may not read the environment, and asks for nothing", async () => {
    me = makeMe({ envs: {} });
    mount();
    await waitFor(() => expect(screen.queryByRole("region", { name: "Janitor" })).toBeNull());
    const calls = (fetch as unknown as { mock: { calls: [Request][] } }).mock.calls.map(
      ([req]) => new URL(req.url).pathname,
    );
    expect(calls).toContain("/api/me");
    expect(calls).not.toContain("/api/env/e1/janitor");
  });
});
