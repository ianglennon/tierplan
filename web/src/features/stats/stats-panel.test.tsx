// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { TooltipProvider } from "@/components/ui/tooltip";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { StatsPanel } from "@/features/stats/stats-panel";
import { HIDDEN_CLOSE_MS } from "@/features/stats/stats-stream";
import "@/i18n";

// The container Stats panel (plan 06-32, RES-11, UI-SPEC "Stats", E8). The stream is a
// FakeEventSource the test drives by hand.

beforeEach(() => {
  FakeEventSource.reset();
});

afterEach(() => {
  cleanup();
  setHidden(false);
  vi.useRealTimers();
});

function setHidden(hidden: boolean): void {
  Object.defineProperty(document, "hidden", { configurable: true, get: () => hidden });
  document.dispatchEvent(new Event("visibilitychange"));
}

const createSource = (url: string) => new FakeEventSource(url);

function renderPanel(extra: Partial<React.ComponentProps<typeof StatsPanel>> = {}) {
  return render(
    <TooltipProvider>
      <StatsPanel
        envId="e1"
        containerId="web-id"
        name="web"
        createSource={createSource}
        {...extra}
      />
    </TooltipProvider>,
  );
}

const source = (): FakeEventSource => {
  const s = FakeEventSource.instances[0];
  if (!s) throw new Error("no stream was opened");
  return s;
};

const MIB = 1024 * 1024;
const READY = { limitBytes: 2 * 1024 * MIB, nanoCpus: null, running: true };
const SAMPLE = {
  t: 1000,
  cpu: 142.4,
  mem: 1.2 * 1024 * MIB,
  netRx: 1.2 * MIB,
  netTx: 340 * 1024,
  blkRead: 0,
  blkWrite: 12 * 1024,
};

function live(ready: object = READY) {
  act(() => source().sendFrame("ready", ready));
}
function sample(body: object = SAMPLE) {
  act(() => source().sendFrame("sample", body));
}

describe("before the first sample", () => {
  it("shows skeleton tiles and says it is waiting, with no live chip yet", () => {
    renderPanel();
    expect(screen.getByText("Waiting for the first sample…")).toBeTruthy();
    expect(screen.queryByText("Live")).toBeNull();
    expect(screen.getByRole("group", { name: "Current values" })).toBeTruthy();
  });

  it("opens one stream for the container on its node", () => {
    renderPanel({ nodeId: "n1" });
    expect(FakeEventSource.instances).toHaveLength(1);
    expect(source().url).toBe("/api/env/e1/containers/web-id/stats/stream?nodeId=n1");
  });
});

describe("the tiles", () => {
  it("shows CPU, memory against its limit, network in and out and block I/O read and write", () => {
    renderPanel();
    live();
    sample();
    expect(screen.getByText("Live")).toBeTruthy();
    expect(screen.queryByText("Waiting for the first sample…")).toBeNull();
    expect(screen.getByText("142%")).toBeTruthy();
    expect(screen.getByText("1.2 GiB")).toBeTruthy();
    expect(screen.getByText("60% of a 2 GiB limit")).toBeTruthy();
    expect(screen.getByText("In 1.2 MiB/s")).toBeTruthy();
    expect(screen.getByText("Out 340 KiB/s")).toBeTruthy();
    expect(screen.getByText("Read 0 B/s")).toBeTruthy();
    expect(screen.getByText("Write 12 KiB/s")).toBeTruthy();
  });

  it("says No limit set for a container without one, and never a percent", () => {
    renderPanel();
    live({ limitBytes: null, nanoCpus: null, running: true });
    sample();
    expect(screen.getByText("No limit set")).toBeTruthy();
    expect(screen.queryByText(/of a .* limit/)).toBeNull();
  });

  it("shows a dash for a CPU the first sample could not tell", () => {
    renderPanel();
    live();
    sample({ ...SAMPLE, cpu: null, netRx: null, netTx: null, blkRead: null, blkWrite: null });
    expect(screen.getAllByText("—").length).toBeGreaterThan(0);
    expect(screen.queryByText(/NaN/)).toBeNull();
  });

  it("explains the CPU number on the label: percent of one core", async () => {
    renderPanel();
    live();
    sample();
    const label = screen.getByRole("button", { name: "CPU" });
    act(() => label.focus());
    expect(
      (await screen.findAllByText("Percent of one core. 100% is one core fully used.")).length,
    ).toBeGreaterThan(0);
  });

  it("keeps a trend beside each tile that no assistive technology reads", () => {
    renderPanel();
    live();
    sample();
    const tiles = screen.getByRole("group", { name: "Current values" });
    const trends = tiles.querySelectorAll("svg[data-testid='sparkline']");
    expect(trends).toHaveLength(4);
    for (const t of trends) expect(t.getAttribute("aria-hidden")).toBe("true");
  });
});

describe("the charts", () => {
  it("draws four, each an image with a summary of now and the highest", async () => {
    renderPanel();
    live();
    sample();
    sample({ ...SAMPLE, t: 3000, cpu: 171 });
    for (const name of [
      /^CPU, now 171%, highest 171% in the last 5 minutes$/,
      /^Memory, now 1\.2 GiB, highest 1\.2 GiB in the last 5 minutes$/,
      /^Network in and out, now In 1\.2 MiB\/s, Out 340 KiB\/s, highest /,
      /^Block I\/O read and write, now Read 0 B\/s, Write 12 KiB\/s, highest /,
    ]) {
      expect(await screen.findByRole("img", { name })).toBeTruthy();
    }
  });
});

describe("the stream's state", () => {
  it("says Reconnecting… when a live stream drops", () => {
    renderPanel();
    live();
    act(() => source().fail());
    expect(screen.getByText("Reconnecting…")).toBeTruthy();
  });

  it("says Paused while this tab is hidden after a minute, and breaks the lines on return", () => {
    vi.useFakeTimers();
    renderPanel();
    live();
    sample();
    sample({ ...SAMPLE, t: 3000 });
    act(() => setHidden(true));
    act(() => {
      vi.advanceTimersByTime(HIDDEN_CLOSE_MS);
    });
    expect(screen.getByText("Paused while this tab is hidden.")).toBeTruthy();
    expect(source().closed).toBe(true);

    act(() => setHidden(false));
    expect(FakeEventSource.instances).toHaveLength(2);
    const next = FakeEventSource.instances[1] as FakeEventSource;
    act(() => next.sendFrame("ready", READY));
    act(() => next.sendFrame("sample", { ...SAMPLE, t: 9000 }));
    const tiles = screen.getByRole("group", { name: "Current values" });
    const path = tiles.querySelector("svg[data-testid='sparkline'] path")?.getAttribute("d") ?? "";
    // Two samples, a gap and one sample: the line starts again after the gap.
    expect(path.match(/M/g)).toHaveLength(2);
    expect(screen.queryByText("Paused while this tab is hidden.")).toBeNull();
  });

  it("warns when the node stops reporting, and clears the warning when it is back", () => {
    renderPanel({ nodeName: "node-1" });
    live();
    sample();
    act(() => source().sendFrame("node", { state: "not_reporting" }));
    expect(
      screen.getByText("node-1 isn't reporting right now. Stats resume when it does."),
    ).toBeTruthy();
    act(() => source().sendFrame("node", { state: "reporting" }));
    expect(screen.queryByText(/isn't reporting right now/)).toBeNull();
  });

  it("says so when the container stops while it is watched", () => {
    renderPanel();
    live();
    sample();
    act(() => source().sendFrame("state", { state: "stopped" }));
    expect(screen.getByText("The container stopped. Stats resume when it starts.")).toBeTruthy();
  });

  it("tells a viewer whose access changed that the stats were closed", () => {
    renderPanel();
    live();
    act(() => source().sendFrame("closed", { reason: "access_lost" }));
    expect(screen.getByText("Your access changed. Stats were closed.")).toBeTruthy();
    expect(screen.queryByText("Live")).toBeNull();
  });

  it("closes the stream and drops the history when the page is left", () => {
    const { unmount } = renderPanel();
    live();
    sample();
    unmount();
    expect(source().closed).toBe(true);
  });
});

describe("a container that is not running", () => {
  it("shows the empty state at once and opens no stream when the page knows it", () => {
    renderPanel({ running: false });
    expect(screen.getByRole("heading", { name: "web isn't running" })).toBeTruthy();
    expect(screen.getByText("Start it to see its stats.")).toBeTruthy();
    expect(FakeEventSource.instances).toHaveLength(0);
  });

  it("shows it when the server says so first", async () => {
    renderPanel();
    act(() => source().sendFrame("ready", { limitBytes: null, nanoCpus: null, running: false }));
    expect(await screen.findByRole("heading", { name: "web isn't running" })).toBeTruthy();
  });

  it("opens the stream when the container starts", async () => {
    const { rerender } = renderPanel({ running: false });
    rerender(
      <TooltipProvider>
        <StatsPanel
          envId="e1"
          containerId="web-id"
          name="web"
          running
          createSource={createSource}
        />
      </TooltipProvider>,
    );
    await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));
    expect(screen.queryByRole("heading", { name: "web isn't running" })).toBeNull();
    expect(within(document.body).getByText("Waiting for the first sample…")).toBeTruthy();
  });
});

describe("the note", () => {
  it("says where the numbers come from and that the history stays in this tab", () => {
    renderPanel();
    expect(
      screen.getByText(
        "Live from each node, every 2 seconds. The last 5 minutes are kept in this browser tab and clear when you leave.",
      ),
    ).toBeTruthy();
    fireEvent.focus(document.body);
  });
});
