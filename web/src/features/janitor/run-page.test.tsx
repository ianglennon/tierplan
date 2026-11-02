// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { janitorRunQuery, RUN_REFETCH_MS } from "@/api/janitor";
import { setCsrfToken } from "@/api/client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { RunPage } from "@/features/janitor/run-page";
import "@/i18n";
import { makeMe } from "@/test-me";
import { run, runItem } from "@/test-janitor";
import { TestRouter } from "@/test-router";

const ENV = "e1";

let runAnswer: () => Response;
let client: QueryClient;

const problem = (type: string, status: number) =>
  new Response(JSON.stringify({ type, title: "Error", status }), {
    status,
    headers: { "Content-Type": "application/problem+json" },
  });

beforeEach(() => {
  setCsrfToken(null);
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  runAnswer = () => Response.json(run());
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      if (url.pathname === "/api/me") return Response.json(makeMe({ envs: { [ENV]: "viewer" } }));
      if (url.pathname === "/api/environments") {
        return Response.json({ items: [{ id: ENV, name: "Prod", envType: "swarm" }] });
      }
      if (url.pathname === `/api/env/${ENV}/janitor/runs/r1`) return runAnswer();
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
          <RunPage envId={ENV} runId="r1" />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

describe("a run that is gone", () => {
  it("says the history limit removed it and links to the run history", async () => {
    runAnswer = () => problem("janitor_run_not_found", 404);
    mount();
    expect(await screen.findByText("This run was removed by the history limit.")).toBeTruthy();
    const link = screen.getByRole("link", { name: "Open run history" });
    expect(link.getAttribute("href")).toBe("/env/e1/janitor#history");
  });
});

describe("a run that can't be read", () => {
  it("says so and offers to try again", async () => {
    runAnswer = () => problem("internal", 500);
    mount();
    expect(await screen.findByText("Couldn't load this run.")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Retry loading run" })).toBeTruthy();
  });
});

describe("the space a run freed", () => {
  it("is a dash when the server could not estimate it", async () => {
    runAnswer = () => Response.json(run({ estBytes: null }));
    mount();
    const label = await screen.findByText("Space freed");
    expect(label.parentElement?.textContent).toBe("Space freed —");
  });

  it("is marked as an estimate when it is known", async () => {
    runAnswer = () => Response.json(run({ estBytes: 1_200_000_000 }));
    mount();
    const label = await screen.findByText("Space freed");
    expect(label.parentElement?.textContent).toBe("Space freed ~1.2 GB");
  });
});

describe("the items of a run", () => {
  it("say in words how each one ended and why", async () => {
    runAnswer = () =>
      Response.json(
        run({
          state: "partial",
          counts: {
            failed: 1,
            kept: 2,
            removedContainers: 1,
            removedImages: 0,
            removedNetworks: 0,
            skippedOffline: 0,
          },
          items: [
            runItem({ seq: 1, name: "web.1.aaa" }),
            runItem({ seq: 2, name: "web.2.bbb", outcome: "refused_in_use", code: "in_use" }),
            runItem({
              seq: 3,
              name: "web.3.ccc",
              outcome: "skipped_protected",
              code: "no_longer_stale",
            }),
            runItem({
              seq: 4,
              name: "web.4.ddd",
              outcome: "error",
              code: "action_failed",
            }),
          ],
        }),
      );
    mount();
    const table = await screen.findByRole("table", { name: "Items in this run" });
    const row = (name: string) => within(table).getByText(name).closest("tr") as HTMLElement;
    expect(within(row("web.1.aaa")).getByText("Removed")).toBeTruthy();
    expect(within(row("web.1.aaa")).getByText("Exited task, 3rd oldest of service web slot 1.")).toBeTruthy();
    expect(within(row("web.2.bbb")).getByText("In use, kept")).toBeTruthy();
    expect(within(row("web.2.bbb")).getByText("web.2.bbb is in use again, so TierPlan kept it.")).toBeTruthy();
    expect(within(row("web.3.ccc")).getByText("Protected")).toBeTruthy();
    expect(within(row("web.3.ccc")).getByText("web.3.ccc is no longer stale, so TierPlan kept it.")).toBeTruthy();
    expect(within(row("web.4.ddd")).getByText("Failed")).toBeTruthy();
    // A partly done run says what was removed, failed and kept.
    expect(screen.getByText("Partly done", { selector: "[data-slot=alert-title]" })).toBeTruthy();
    expect(
      screen.getByText("Removed 1 of 4. 1 failed and 2 were kept. Open an item to see why."),
    ).toBeTruthy();
  });

  it("draw a name that holds markup as text", async () => {
    runAnswer = () =>
      Response.json(run({ items: [runItem({ name: "<script>alert(1)</script>" })] }));
    mount();
    const table = await screen.findByRole("table", { name: "Items in this run" });
    expect(within(table).getByText("<script>alert(1)</script>")).toBeTruthy();
    expect(table.querySelector("script")).toBeNull();
  });

  it("say when a run found nothing to remove", async () => {
    runAnswer = () => Response.json(run({ items: [] }));
    mount();
    expect(await screen.findByText("This run found nothing to remove.")).toBeTruthy();
  });
});

describe("a run that failed", () => {
  it("is a destructive alert with the reason and what happens next", async () => {
    runAnswer = () => Response.json(run({ state: "failed", reason: "failed" }));
    mount();
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("The cleanup failed");
    expect(alert.textContent).toContain(
      "TierPlan couldn't read the environment. Nothing more was removed. TierPlan will try again at the next scheduled run.",
    );
  });
});

describe("following a run", () => {
  it("reads it again every two seconds while it waits or works, and stops when it has ended", () => {
    const opts = janitorRunQuery(ENV, "r1");
    const interval = opts.refetchInterval as (q: { state: { data?: { state: string } } }) => unknown;
    expect(RUN_REFETCH_MS).toBe(2000);
    expect(interval({ state: { data: { state: "waiting" } } })).toBe(RUN_REFETCH_MS);
    expect(interval({ state: { data: { state: "running" } } })).toBe(RUN_REFETCH_MS);
    expect(interval({ state: { data: { state: "completed" } } })).toBe(false);
    expect(interval({ state: { data: { state: "partial" } } })).toBe(false);
    expect(interval({ state: {} })).toBe(false);
  });
});
