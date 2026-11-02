// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { act, cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it } from "vitest";

import { TooltipProvider } from "@/components/ui/tooltip";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { ServiceStatsPanel } from "@/features/stats/stats-panel";
import "@/i18n";

// The service Stats tab (plan 06-33, RES-11, D-29, UI-SPEC "Stats", E8): the sum over the
// running tasks, the table by task, and what is left out of the totals. The stream is a
// FakeEventSource the test drives by hand.

beforeEach(() => FakeEventSource.reset());
afterEach(cleanup);

const createSource = (url: string) => new FakeEventSource(url);

function renderPanel() {
  return render(
    <TooltipProvider>
      <ServiceStatsPanel envId="e1" serviceId="svc-web" name="web" createSource={createSource} />
    </TooltipProvider>,
  );
}

const source = (): FakeEventSource => {
  const s = FakeEventSource.instances[0];
  if (!s) throw new Error("no stream was opened");
  return s;
};

const MIB = 1024 * 1024;
const GIB = 1024 * MIB;

const TASKS = [
  { id: "w1aaaaaaaa", slot: 1, node: "mgr-1", nodeId: "n1" },
  { id: "w2bbbbbbbb", slot: 2, node: "mgr-1", nodeId: "n1" },
  { id: "w3cccccccc", slot: 3, node: "wrk-1", nodeId: "n3" },
];

const ts = (id: string, extra: Record<string, unknown> = {}) => ({
  id,
  cpu: 100,
  mem: 600 * MIB,
  netRx: 1 * MIB,
  netTx: 100 * 1024,
  blkRead: 0,
  blkWrite: 0,
  noData: false,
  ...extra,
});

const SAMPLE = {
  t: 1000,
  total: {
    cpu: 200,
    mem: 1.2 * GIB,
    netRx: 2 * MIB,
    netTx: 200 * 1024,
    blkRead: 0,
    blkWrite: 0,
    counted: 2,
    silent: 1,
  },
  tasks: [
    ts("w1aaaaaaaa"),
    ts("w2bbbbbbbb"),
    ts("w3cccccccc", { cpu: null, mem: null, netRx: null, netTx: null, noData: true }),
  ],
};

function ready(extra: Record<string, unknown> = {}) {
  act(() => source().sendFrame("ready", { tasks: TASKS, limitBytes: null, ...extra }));
}
function sample(body: object = SAMPLE) {
  act(() => source().sendFrame("sample", body));
}

describe("the stream", () => {
  it("opens one stream for the service and waits for the first sample", () => {
    renderPanel();
    expect(FakeEventSource.instances).toHaveLength(1);
    expect(source().url).toBe("/api/env/e1/services/svc-web/stats/stream");
    expect(screen.getByText("Waiting for the first sample…")).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "By task" })).toBeNull();
  });

  it("closes the stream when the page is left", () => {
    const { unmount } = renderPanel();
    unmount();
    expect(source().closed).toBe(true);
  });
});

describe("the totals", () => {
  it("are the sum over the tasks that answered", () => {
    renderPanel();
    ready();
    sample();
    const tiles = screen.getByRole("group", { name: "Current values" });
    expect(within(tiles).getByText("200%")).toBeTruthy();
    expect(within(tiles).getByText("1.2 GiB")).toBeTruthy();
    expect(within(tiles).getByText("In 2 MiB/s")).toBeTruthy();
    expect(within(tiles).getByText("Out 200 KiB/s")).toBeTruthy();
    expect(screen.getByText("Live")).toBeTruthy();
  });

  it("give a percent of the limit only when ready says every task shares one", () => {
    renderPanel();
    ready({ limitBytes: 1 * GIB });
    sample();
    // 1.2 GiB against two counted limits of 1 GiB.
    expect(screen.getByText("60% of 2 GiB in limits")).toBeTruthy();
  });

  it("say there is no shared limit otherwise, and never a percent", () => {
    renderPanel();
    ready({ limitBytes: null });
    sample();
    expect(screen.getByText("No shared limit")).toBeTruthy();
    expect(screen.queryByText(/in limits/)).toBeNull();
  });

  it("are dashes when no task answered", () => {
    renderPanel();
    ready();
    sample({
      t: 1,
      total: {
        cpu: null,
        mem: 0,
        netRx: null,
        netTx: null,
        blkRead: null,
        blkWrite: null,
        counted: 0,
        silent: 3,
      },
      tasks: TASKS.map((t) => ts(t.id, { cpu: null, mem: null, noData: true })),
    });
    const tiles = screen.getByRole("group", { name: "Current values" });
    expect(within(tiles).getAllByText("—").length).toBeGreaterThan(0);
    expect(within(tiles).queryByText("0 B")).toBeNull();
  });
});

describe("the table by task", () => {
  it("has a row for each running task, with its node and its numbers", () => {
    renderPanel();
    ready();
    sample();
    expect(screen.getByRole("heading", { name: "By task" })).toBeTruthy();
    const rows = screen.getAllByRole("row").slice(1);
    expect(rows).toHaveLength(3);
    expect(within(rows[0] as HTMLElement).getByText("web.1")).toBeTruthy();
    expect(within(rows[0] as HTMLElement).getByText("mgr-1")).toBeTruthy();
    expect(within(rows[0] as HTMLElement).getByText("100%")).toBeTruthy();
  });

  it("says No data for the task on the node that is not reporting", () => {
    renderPanel();
    ready();
    sample();
    const row = screen.getByRole("row", { name: /web\.3/ });
    expect(within(row).getByText("No data")).toBeTruthy();
  });

  it("fills the table from the ready event before the first sample, with dashes", () => {
    renderPanel();
    ready();
    const rows = screen.getAllByRole("row").slice(1);
    expect(rows).toHaveLength(3);
    expect(screen.queryByText("No data")).toBeNull();
  });

  it("follows the tasks event: a task that joins gets a row and one that stopped loses it", () => {
    renderPanel();
    ready();
    sample();
    act(() =>
      source().sendFrame("tasks", {
        tasks: [TASKS[0], { id: "w4dddddddd", slot: 4, node: "mgr-1", nodeId: "n1" }],
        limitBytes: null,
      }),
    );
    const names = screen
      .getAllByRole("row")
      .slice(1)
      .map((r) => r.textContent);
    expect(names.some((n) => n?.includes("web.4"))).toBe(true);
    expect(names.some((n) => n?.includes("web.2"))).toBe(false);
  });

  it("keeps a trend of the CPU of each task", () => {
    renderPanel();
    ready();
    sample();
    sample({ ...SAMPLE, t: 3000 });
    const paths = Array.from(
      screen.getByRole("table").querySelectorAll("svg[data-testid='sparkline'] path"),
    );
    expect(paths).toHaveLength(3);
    // Two samples of the first task make a line; the task with no data has none.
    expect(paths[0]?.getAttribute("d")).toMatch(/^M.*L/);
    expect(paths[2]?.getAttribute("d")).toBe("");
  });

  it("shows one row for one task", () => {
    renderPanel();
    act(() => source().sendFrame("ready", { tasks: [TASKS[0]], limitBytes: null }));
    expect(screen.getAllByRole("row").slice(1)).toHaveLength(1);
  });
});

describe("what is left out", () => {
  it("is said in an alert that counts the tasks on nodes that are not reporting", () => {
    renderPanel();
    ready();
    sample();
    expect(
      screen.getByText("1 task runs on a node that isn't reporting, so these totals leave it out."),
    ).toBeTruthy();
    sample({ ...SAMPLE, total: { ...SAMPLE.total, silent: 2 } });
    expect(
      screen.getByText(
        "2 tasks run on nodes that aren't reporting, so these totals leave them out.",
      ),
    ).toBeTruthy();
  });

  it("is not said when every task is counted", () => {
    renderPanel();
    ready();
    sample({ ...SAMPLE, total: { ...SAMPLE.total, silent: 0, counted: 3 } });
    expect(screen.queryByText(/aren't reporting, so these totals/)).toBeNull();
    expect(screen.queryByText(/isn't reporting, so these totals/)).toBeNull();
  });

  it("includes the line that stopped tasks drop out of the totals", () => {
    renderPanel();
    ready();
    sample();
    expect(screen.getByText("Stopped tasks drop out of the totals.")).toBeTruthy();
  });
});

describe("a service with no running task", () => {
  it("says No running tasks, with no table", () => {
    renderPanel();
    act(() => source().sendFrame("ready", { tasks: [], limitBytes: null }));
    act(() => source().sendFrame("sample", { t: 1, total: SAMPLE.total, tasks: [] }));
    expect(screen.getByRole("heading", { name: "No running tasks" })).toBeTruthy();
    expect(screen.getByText("Stats appear here once the service has a running task.")).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "By task" })).toBeNull();
  });

  it("shows the tiles once a task starts", () => {
    renderPanel();
    act(() => source().sendFrame("ready", { tasks: [], limitBytes: null }));
    act(() => source().sendFrame("tasks", { tasks: [TASKS[0]], limitBytes: null }));
    expect(screen.queryByRole("heading", { name: "No running tasks" })).toBeNull();
    expect(screen.getByRole("heading", { name: "By task" })).toBeTruthy();
  });
});

describe("the stream's state", () => {
  it("tells a viewer whose access changed that the stats were closed", () => {
    renderPanel();
    ready();
    act(() => source().sendFrame("closed", { reason: "access_lost" }));
    expect(screen.getByText("Your access changed. Stats were closed.")).toBeTruthy();
  });

  it("says Reconnecting… when a live stream drops", () => {
    renderPanel();
    ready();
    act(() => source().fail());
    expect(screen.getByText("Reconnecting…")).toBeTruthy();
  });

  it("does not claim a service that was removed is still live", () => {
    renderPanel();
    ready();
    act(() => source().sendFrame("closed", { reason: "gone" }));
    expect(screen.queryByText("Live")).toBeNull();
  });
});
