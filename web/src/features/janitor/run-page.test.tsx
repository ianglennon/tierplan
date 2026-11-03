// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { setCsrfToken } from "@/api/client";
import type { JanitorJobFrame, JanitorJobItemFrame } from "@/api/janitor";
import { janitorRunQuery, RUN_REFETCH_MS, runPollMs } from "@/api/janitor";
import { TooltipProvider } from "@/components/ui/tooltip";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import type { RunSearch, RunSearchPatch } from "@/features/janitor/run-items-table";
import { RunPage } from "@/features/janitor/run-page";
import "@/i18n";
import { run, runItem } from "@/test-janitor";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

const ENV = "e1";

let runAnswer: () => Response;
// What the stream address answers when it is asked for by hand (the probe of a refused stream).
let streamStatus = 200;
let client: QueryClient;

const problem = (type: string, status: number) =>
  new Response(JSON.stringify({ type, title: "Error", status }), {
    status,
    headers: { "Content-Type": "application/problem+json" },
  });

beforeEach(() => {
  setCsrfToken(null);
  FakeEventSource.reset();
  vi.stubGlobal("EventSource", FakeEventSource);
  streamStatus = 200;
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  runAnswer = () => Response.json(run());
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request | string) => {
      // The probe of a refused stream asks by address, the API client by Request.
      const url = new URL(typeof req === "string" ? req : req.url, "http://localhost");
      if (url.pathname === "/api/me") return Response.json(makeMe({ envs: { [ENV]: "viewer" } }));
      if (url.pathname === "/api/environments") {
        return Response.json({ items: [{ id: ENV, name: "Prod", envType: "swarm" }] });
      }
      if (url.pathname === `/api/env/${ENV}/janitor/runs/r1`) return runAnswer();
      if (url.pathname === `/api/env/${ENV}/janitor/runs/r1/stream`) {
        return problem("janitor_job_not_found", streamStatus);
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

function mount(search?: RunSearch & { onSearchChange?: (patch: RunSearchPatch) => void }) {
  const { onSearchChange, ...current } = search ?? {};
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <RunPage
            envId={ENV}
            runId="r1"
            search={search ? current : undefined}
            onSearchChange={onSearchChange}
          />
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
    expect(
      within(row("web.1.aaa")).getByText("Exited task, 3rd oldest of service web slot 1."),
    ).toBeTruthy();
    expect(within(row("web.2.bbb")).getByText("In use, kept")).toBeTruthy();
    expect(
      within(row("web.2.bbb")).getByText("web.2.bbb is in use again, so TierPlan kept it."),
    ).toBeTruthy();
    expect(within(row("web.3.ccc")).getByText("Protected")).toBeTruthy();
    expect(
      within(row("web.3.ccc")).getByText("web.3.ccc is no longer stale, so TierPlan kept it."),
    ).toBeTruthy();
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
    const interval = opts.refetchInterval as (q: {
      state: { data?: { state: string } };
    }) => unknown;
    expect(RUN_REFETCH_MS).toBe(2000);
    expect(interval({ state: { data: { state: "waiting" } } })).toBe(RUN_REFETCH_MS);
    expect(interval({ state: { data: { state: "running" } } })).toBe(RUN_REFETCH_MS);
    expect(interval({ state: { data: { state: "completed" } } })).toBe(false);
    expect(interval({ state: { data: { state: "partial" } } })).toBe(false);
    expect(interval({ state: {} })).toBe(false);
  });
});

describe("the poll of a run", () => {
  it("is slower while the stream is live, and never once the run has ended", () => {
    expect(runPollMs("running", true)).toBe(5000);
    expect(runPollMs("waiting", false)).toBe(RUN_REFETCH_MS);
    expect(runPollMs("completed", true)).toBe(false);
    expect(runPollMs(undefined, false)).toBe(false);
  });
});

const jobItem = (n: number, over: Partial<JanitorJobItemFrame> = {}): JanitorJobItemFrame => ({
  id: `container/n1/c${n}`,
  label: `web.${n}.abc`,
  kind: "container",
  state: "waiting",
  code: "",
  message: "",
  result: {},
  ...over,
});

const jobFrame = (over: Partial<JanitorJobFrame> = {}): JanitorJobFrame => ({
  id: "j1",
  runId: "r1",
  state: "running",
  trigger: "manual",
  by: "Ada",
  createdAt: new Date().toISOString(),
  finishedAt: "",
  done: 0,
  total: 4,
  items: [1, 2, 3, 4].map((n) => jobItem(n)),
  ...over,
});

const runningRun = () =>
  run({
    state: "running",
    finishedAt: null,
    items: [runItem({ seq: 1, name: "web.1.abc" })],
  });

const announced = () => document.querySelector("[data-slot=run-announcer]")?.textContent ?? "";

describe("RunPageLive", () => {
  it("follows the stream while the run works and settles into its record when it ends", async () => {
    runAnswer = () => Response.json(runningRun());
    mount();
    await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));
    const source = FakeEventSource.instances[0] as FakeEventSource;
    expect(source.url).toBe(`/api/env/${ENV}/janitor/runs/r1/stream`);

    act(() => source.sendFrame("snapshot", jobFrame()));
    const bar = await screen.findByRole("progressbar", { name: "Cleanup progress" });
    expect(bar.getAttribute("aria-valuemin")).toBe("0");
    expect(bar.getAttribute("aria-valuemax")).toBe("100");
    expect(bar.getAttribute("aria-valuenow")).toBe("0");
    expect(bar.getAttribute("aria-valuetext")).toBe("Removing 0 of 4");
    expect(screen.getByText("Live")).toBeTruthy();
    expect(screen.getByText("Running")).toBeTruthy();

    act(() => source.sendFrame("item", jobItem(1, { state: "done" }), "1"));
    act(() => source.sendFrame("item", jobItem(2, { state: "done" }), "2"));
    expect(screen.getByRole("progressbar").getAttribute("aria-valuetext")).toBe("Removing 2 of 4");
    expect(screen.getByRole("progressbar").getAttribute("aria-valuenow")).toBe("50");
    expect(screen.getByText(/Removing 2 of 4…/)).toBeTruthy();

    // The removal in hand is named, in mono.
    act(() => source.sendFrame("item", jobItem(3, { state: "running" }), "3"));
    const current = screen.getByText("web.3.abc");
    expect(current.className).toContain("font-mono");

    // The polite region spoke once for each quarter reached (25% and 50%), and nothing else.
    expect(announced()).toBe("Removing 1 of 4Removing 2 of 4");

    // The run ends: the record is read again and the page becomes the record, with no progress.
    runAnswer = () =>
      Response.json(
        run({
          state: "completed",
          counts: {
            failed: 0,
            kept: 0,
            removedContainers: 4,
            removedImages: 0,
            removedNetworks: 0,
            skippedOffline: 0,
          },
          items: [1, 2, 3, 4].map((n) => runItem({ seq: n, name: `web.${n}.abc` })),
        }),
      );
    act(() => source.sendFrame("done", jobFrame({ state: "done", finishedAt: "x" }), "9"));
    expect(await screen.findByText("Completed")).toBeTruthy();
    expect(screen.queryByRole("progressbar")).toBeNull();
    expect(screen.queryByText("Live")).toBeNull();
    const table = await screen.findByRole("table", { name: "Items in this run" });
    expect(within(table).getAllByRole("row")).toHaveLength(5);
    // The end is announced once.
    await waitFor(() =>
      expect(announced()).toBe(
        "Removing 1 of 4Removing 2 of 4Cleanup completed. Removed 4 containers.",
      ),
    );
  });

  it("says a waiting run waits and gives the time it tries for", async () => {
    runAnswer = () => Response.json(run({ state: "waiting", finishedAt: null, items: [] }));
    mount();
    expect(
      await screen.findByText(
        "Waiting for a deploy to finish. TierPlan tries for about 10 minutes, then skips this run.",
      ),
    ).toBeTruthy();
    expect(screen.getByText("Waiting for a deploy")).toBeTruthy();
    // The job has no items yet, so there is nothing to count.
    await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));
    act(() =>
      (FakeEventSource.instances[0] as FakeEventSource).sendFrame(
        "snapshot",
        jobFrame({ total: 0, items: [] }),
      ),
    );
    expect(screen.queryByRole("progressbar")).toBeNull();
  });

  it("says it is reconnecting when the stream drops, and keeps the last progress", async () => {
    runAnswer = () => Response.json(runningRun());
    mount();
    await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));
    const source = FakeEventSource.instances[0] as FakeEventSource;
    act(() => source.sendFrame("snapshot", jobFrame()));
    await screen.findByText("Live");
    act(() => source.fail());
    expect(await screen.findByText("Reconnecting…")).toBeTruthy();
    expect(screen.getByRole("progressbar")).toBeTruthy();
  });

  it("reads the record alone when the server no longer holds the job", async () => {
    streamStatus = 404;
    runAnswer = () => Response.json(runningRun());
    mount();
    await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));
    act(() => (FakeEventSource.instances[0] as FakeEventSource).fail(true));
    expect(await screen.findByText("Running")).toBeTruthy();
    await waitFor(() => expect(screen.queryByText("Reconnecting…")).toBeNull());
    expect(screen.queryByRole("progressbar")).toBeNull();
    expect(FakeEventSource.instances).toHaveLength(1);
  });

  it("opens no stream for a run that has ended", async () => {
    runAnswer = () => Response.json(run({ items: [runItem()] }));
    mount();
    await screen.findByRole("table", { name: "Items in this run" });
    expect(FakeEventSource.instances).toHaveLength(0);
    expect(announced()).toBe("");
  });
});

const counts = (over: Partial<ReturnType<typeof run>["counts"]> = {}) => ({
  failed: 0,
  kept: 0,
  removedContainers: 0,
  removedImages: 0,
  removedNetworks: 0,
  skippedOffline: 0,
  ...over,
});

describe("the summary of a run", () => {
  it("counts what was removed, the size when it is known, what was kept and what failed", async () => {
    runAnswer = () =>
      Response.json(
        run({
          counts: counts({
            removedContainers: 14,
            removedNetworks: 3,
            removedImages: 2,
            kept: 1,
            failed: 4,
          }),
          estBytes: 1_200_000_000,
          items: [runItem()],
        }),
      );
    mount();
    expect(
      await screen.findByText(
        "Removed 14 containers, 3 networks, 2 images, ~1.2 GB. Kept 1. Failed 4.",
      ),
    ).toBeTruthy();
  });

  it("leaves out a part that is zero, and names the one that is left in the right number", async () => {
    runAnswer = () =>
      Response.json(
        run({ counts: counts({ removedNetworks: 1 }), estBytes: null, items: [runItem()] }),
      );
    mount();
    expect(await screen.findByText("Removed 1 network. Kept 0. Failed 0.")).toBeTruthy();
  });

  it("says nothing was removed when nothing was", async () => {
    runAnswer = () => Response.json(run({ items: [runItem({ outcome: "refused_in_use" })] }));
    mount();
    expect(await screen.findByText("Nothing removed. Kept 0. Failed 0.")).toBeTruthy();
  });
});

describe("the alert of a run that did not complete", () => {
  it("is a polite warning for a partly done run, with the counts", async () => {
    runAnswer = () =>
      Response.json(
        run({
          state: "partial",
          counts: counts({ removedContainers: 14, failed: 2, kept: 3 }),
          items: Array.from({ length: 19 }, (_, i) => runItem({ seq: i + 1, name: `t-${i}` })),
        }),
      );
    mount();
    const alert = await screen.findByText("Partly done", { selector: "[data-slot=alert-title]" });
    const box = alert.closest("[data-slot=alert]") as HTMLElement;
    expect(box.getAttribute("role")).toBe("status");
    expect(box.textContent).toContain(
      "Removed 14 of 19. 2 failed and 3 were kept. Open an item to see why.",
    );
  });

  it("is a polite note with the reason for a skipped run", async () => {
    runAnswer = () => Response.json(run({ state: "skipped", reason: "deploy_in_progress" }));
    mount();
    const title = await screen.findByText("Skipped", { selector: "[data-slot=alert-title]" });
    const box = title.closest("[data-slot=alert]") as HTMLElement;
    expect(box.getAttribute("role")).toBe("status");
    expect(box.textContent).toContain(
      "A deploy was in progress. TierPlan will try again at the next scheduled run.",
    );
  });

  it("is a destructive alert for a run a restart cut short, naming the restart", async () => {
    runAnswer = () => Response.json(run({ state: "failed", reason: "server_restarted" }));
    mount();
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("The cleanup failed");
    expect(alert.textContent).toContain(
      "TierPlan restarted during this run. Nothing more was removed. TierPlan will try again at the next scheduled run.",
    );
  });

  it("does not begin its sentence with a stop when the failure has no reason", async () => {
    runAnswer = () => Response.json(run({ state: "failed", reason: "" }));
    mount();
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain(
      "Nothing more was removed. TierPlan will try again at the next scheduled run.",
    );
    expect(alert.textContent).not.toContain(". Nothing");
  });
});

describe("the filters of a run", () => {
  const items = () => [
    runItem({ seq: 1, name: "web.1.abc" }),
    runItem({ seq: 2, name: "web.2.def", outcome: "error", code: "action_failed" }),
    runItem({ seq: 3, name: "db.1.ghi", outcome: "skipped_node_offline", code: "node_offline" }),
    runItem({ seq: 4, name: "db.2.jkl", outcome: "refused_in_use", code: "in_use" }),
  ];
  const shown = () => {
    const table = screen.getByRole("table", { name: "Items in this run" });
    return within(table)
      .getAllByRole("row")
      .slice(1)
      .map((r) => within(r).getAllByRole("cell")[1]?.textContent);
  };

  it("offers All, Removed, Kept and Problems as one choice, and lists the problems on request", async () => {
    runAnswer = () => Response.json(run({ items: items() }));
    mount();
    await screen.findByRole("table", { name: "Items in this run" });
    const group = screen.getByRole("group", { name: "Show" });
    expect(
      within(group)
        .getAllByRole("radio")
        .map((r) => r.textContent),
    ).toEqual(["All", "Removed", "Kept", "Problems"]);
    expect(within(group).getByRole("radio", { name: "All" }).getAttribute("aria-checked")).toBe(
      "true",
    );
    expect(shown()).toHaveLength(4);
    fireEvent.click(within(group).getByRole("radio", { name: "Problems" }));
    expect(shown()).toEqual(["web.2.def", "db.1.ghi"]);
    expect(
      within(group).getByRole("radio", { name: "Problems" }).getAttribute("aria-checked"),
    ).toBe("true");
    // Choosing the chosen one again is not a choice: it stays.
    fireEvent.click(within(group).getByRole("radio", { name: "Problems" }));
    expect(shown()).toHaveLength(2);
  });

  it("searches by name through the box, which can be cleared", async () => {
    runAnswer = () => Response.json(run({ items: items() }));
    mount();
    await screen.findByRole("table", { name: "Items in this run" });
    const box = screen.getByRole("searchbox", { name: "Search items" });
    expect(box.getAttribute("placeholder")).toBe("Search by name");
    fireEvent.change(box, { target: { value: "db." } });
    await waitFor(() => expect(shown()).toEqual(["db.1.ghi", "db.2.jkl"]));
    fireEvent.click(screen.getByRole("button", { name: "Clear search" }));
    await waitFor(() => expect(shown()).toHaveLength(4));
  });

  it("starts from the filter the address names", async () => {
    runAnswer = () => Response.json(run({ items: items() }));
    mount({ show: "problems", q: "db" });
    await screen.findByRole("table", { name: "Items in this run" });
    expect(shown()).toEqual(["db.1.ghi"]);
    expect(screen.getByRole("radio", { name: "Problems" }).getAttribute("aria-checked")).toBe(
      "true",
    );
    expect((screen.getByRole("searchbox") as HTMLInputElement).value).toBe("db");
  });

  it("reports a choice to the address and takes the default out of it", async () => {
    runAnswer = () => Response.json(run({ items: items() }));
    const onSearchChange = vi.fn();
    mount({ show: "problems", onSearchChange });
    await screen.findByRole("table", { name: "Items in this run" });
    fireEvent.click(screen.getByRole("radio", { name: "Kept" }));
    expect(onSearchChange).toHaveBeenLastCalledWith({ show: "kept" });
    fireEvent.click(screen.getByRole("radio", { name: "All" }));
    expect(onSearchChange).toHaveBeenLastCalledWith({ show: undefined });
  });

  it("shows no filters for a run that found nothing", async () => {
    runAnswer = () => Response.json(run({ items: [] }));
    mount();
    expect(await screen.findByText("This run found nothing to remove.")).toBeTruthy();
    expect(screen.queryByRole("group", { name: "Show" })).toBeNull();
  });
});
