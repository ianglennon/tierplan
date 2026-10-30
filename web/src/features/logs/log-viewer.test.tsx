// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import type { ComponentProps } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { TooltipProvider } from "@/components/ui/tooltip";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import type { LogLineData } from "@/features/logs/log-buffer";
import { LogViewer, popoutLogsUrl } from "@/features/logs/log-viewer";
import type { FilterOptions } from "@/features/logs/task-filter-bar";
import "@/i18n";

// The live log viewer (plan 04-19, OBS-01, D-15, D-16, T-04-67). jsdom lays nothing
// out, so the surface is given a height and the scrolling is observed through the
// calls the virtualiser makes.

const VIEWPORT = 400;
const LINE = 20;

const proto = HTMLElement.prototype as unknown as Record<string, unknown>;
const saved: Record<string, PropertyDescriptor | undefined> = {};

function lay(prop: string, value: number) {
  saved[prop] = Object.getOwnPropertyDescriptor(HTMLElement.prototype, prop);
  Object.defineProperty(HTMLElement.prototype, prop, { configurable: true, value });
}

beforeEach(() => {
  FakeEventSource.reset();
  vi.stubGlobal("EventSource", FakeEventSource);
  lay("offsetHeight", VIEWPORT);
  lay("offsetWidth", 800);
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  localStorage.clear();
  for (const [prop, desc] of Object.entries(saved)) {
    if (desc) Object.defineProperty(HTMLElement.prototype, prop, desc);
    else delete proto[prop];
  }
});

function source(): FakeEventSource {
  const s = FakeEventSource.instances[0];
  if (!s) throw new Error("no stream was opened");
  return s;
}

function lines(n: number, from = 0): LogLineData[] {
  return Array.from({ length: n }, (_, i) => ({
    t: `2026-10-09T12:00:${String(from + i).padStart(2, "0")}.000000000Z`,
    s: "o" as const,
    m: `line ${from + i}`,
  }));
}

function renderViewer(props: Partial<ComponentProps<typeof LogViewer>> = {}) {
  return render(<LogViewer envId="e1" containerId="web-id" nodeId="n1" name="web" {...props} />);
}

async function connect() {
  await act(async () => {
    source().open();
    source().sendFrame("ready", { tty: false });
  });
}

async function send(batch: LogLineData[]) {
  await act(async () => {
    source().sendFrame("log", { lines: batch }, batch[batch.length - 1]?.t ?? "");
  });
}

describe("opening the viewer", () => {
  it("opens the stream of the container on the last 500 lines, with a viewer ID of 22 URL-safe characters", () => {
    renderViewer();
    const url = new URL(source().url, "http://x");
    expect(url.pathname).toBe("/api/env/e1/containers/web-id/logs/stream");
    expect(url.searchParams.get("nodeId")).toBe("n1");
    expect(url.searchParams.get("tail")).toBe("500");
    expect(url.searchParams.get("sid")).toMatch(/^[A-Za-z0-9_-]{22}$/);
  });

  it("shows skeleton lines in the surface and no Live chip until the stream is connected", async () => {
    renderViewer();
    const surface = screen.getByRole("log", { name: "Logs for web" });
    expect(surface.getAttribute("aria-busy")).toBe("true");
    expect(surface.querySelectorAll('[data-slot="skeleton"]').length).toBeGreaterThan(3);
    expect(screen.queryByText("Live")).toBeNull();
    expect(screen.queryByText("No log output yet")).toBeNull();

    await connect();
    expect(surface.getAttribute("aria-busy")).toBe("false");
    expect(surface.querySelectorAll('[data-slot="skeleton"]').length).toBe(0);
    expect(screen.getByText("Live")).toBeTruthy();
  });

  it("says there is no log output yet when the stream is connected and nothing has come", async () => {
    renderViewer();
    await connect();
    expect(screen.getByText("No log output yet")).toBeTruthy();
    await send(lines(1));
    expect(screen.queryByText("No log output yet")).toBeNull();
  });

  it("is a focusable log region that does not announce new lines", () => {
    renderViewer();
    const surface = screen.getByRole("log", { name: "Logs for web" });
    expect(surface.getAttribute("tabindex")).toBe("0");
    expect(surface.getAttribute("aria-live")).toBe("off");
  });
});

describe("the lines", () => {
  it("draws the lines as text, marks stderr in the gutter and leaves stdout's gutter empty", async () => {
    renderViewer();
    await connect();
    await send([
      { t: "2026-10-09T12:00:00.000000000Z", s: "o", m: "all is well" },
      { t: "2026-10-09T12:00:01.000000000Z", s: "e", m: "something broke" },
    ]);
    const surface = screen.getByRole("log");
    expect(surface.textContent).toContain("all is well");
    const rows = surface.querySelectorAll("[data-stream]");
    expect(rows).toHaveLength(2);
    expect(rows[0]?.getAttribute("data-stream")).toBe("stdout");
    expect(rows[0]?.textContent).toBe("all is well");
    expect(rows[1]?.getAttribute("data-stream")).toBe("stderr");
    expect(rows[1]?.textContent).toBe("stderrsomething broke");
    // The marker is text, and it cannot be selected, so a copy stays clean.
    const marker = screen.getByText("stderr");
    expect(marker.className).toContain("select-none");
    expect(screen.getAllByText("stderr")).toHaveLength(1);
  });

  it("never reads a line as markup or turns it into a link", async () => {
    renderViewer();
    await connect();
    const nasty =
      '<img src=x onerror="alert(1)"> <a href="https://evil.test">click</a> https://evil.test';
    await send([{ t: "2026-10-09T12:00:00.000000000Z", s: "o", m: nasty }]);
    const surface = screen.getByRole("log");
    expect(surface.querySelector("img")).toBeNull();
    expect(surface.querySelector("a")).toBeNull();
    expect(surface.textContent).toContain(nasty);
  });

  it("draws only the rows in view of a long log", async () => {
    renderViewer();
    await connect();
    await send(lines(2000));
    const rows = screen.getByRole("log").querySelectorAll("[data-stream]");
    expect(rows.length).toBeGreaterThan(0);
    expect(rows.length).toBeLessThan(200);
  });
});

describe("following", () => {
  it("moves to the end as lines arrive", async () => {
    renderViewer();
    await connect();
    const surface = screen.getByRole("log");
    await send(lines(100));
    // The browser clamps this to the real end; the viewer asks for the whole height:
    // the lines and the "Load earlier lines" row above them.
    expect(surface.scrollTop).toBe((100 + 1) * LINE);
    await send(lines(10, 100));
    expect(surface.scrollTop).toBe((110 + 1) * LINE);
  });

  it("stops following when the viewer scrolls away from the end and follows again at the end", async () => {
    renderViewer();
    await connect();
    await send(lines(100));
    const surface = screen.getByRole("log");
    Object.defineProperty(surface, "scrollHeight", { configurable: true, value: 100 * LINE });
    Object.defineProperty(surface, "clientHeight", { configurable: true, value: VIEWPORT });

    surface.scrollTop = 200;
    fireEvent.scroll(surface);
    await send(lines(10, 100));
    expect(surface.scrollTop).toBe(200);

    surface.scrollTop = 100 * LINE - VIEWPORT;
    fireEvent.scroll(surface);
    await send(lines(10, 110));
    expect(surface.scrollTop).toBe((120 + 1) * LINE);
  });
});

describe("the end of the stream", () => {
  it("closes the connection when the viewer goes away", async () => {
    const { unmount } = renderViewer();
    await connect();
    expect(source().closed).toBe(false);
    unmount();
    expect(source().closed).toBe(true);
  });

  it("says the access changed and keeps the lines, when the server closes the stream for it", async () => {
    renderViewer();
    await connect();
    await send(lines(3));
    await act(async () => {
      source().sendFrame("closed", { reason: "access_lost" });
    });
    expect(screen.getByText("Your access changed. Logs were closed.")).toBeTruthy();
    expect(screen.queryByText("Live")).toBeNull();
    expect(screen.getByRole("log").textContent).toContain("line 2");
  });

  it("shows Reconnecting while it retries a dropped stream, keeps the lines, and shows Live again once it is back", async () => {
    vi.useFakeTimers();
    try {
      renderViewer();
      await connect();
      await send(lines(2));
      await act(async () => {
        source().fail();
      });
      expect(screen.getByText("Reconnecting…")).toBeTruthy();
      expect(screen.getByRole("log").textContent).toContain("line 1");
      await act(async () => {
        vi.advanceTimersByTime(1000);
      });
      const again = FakeEventSource.instances[1] as FakeEventSource;
      expect(new URL(again.url, "http://x").searchParams.get("after")).toBe(lines(2)[1]?.t);
      await act(async () => {
        again.open();
        again.sendFrame("ready", { tty: false });
        // The seam: line 1 is held already, line 2 is new.
        again.sendFrame("log", { lines: [...lines(2).slice(1), ...lines(1, 2)] });
      });
      expect(screen.getByText("Live")).toBeTruthy();
      const rows = Array.from(screen.getByRole("log").querySelectorAll("[data-stream]")).map(
        (r) => r.textContent,
      );
      expect(rows).toEqual(["line 0", "line 1", "line 2"]);
    } finally {
      vi.useRealTimers();
    }
  });

  it("adds one row, Logs were interrupted, when the stream cannot be resumed because the container is gone", async () => {
    vi.useFakeTimers();
    vi.stubGlobal(
      "fetch",
      vi.fn(
        async () =>
          new Response(JSON.stringify({ type: "container_not_found", status: 404 }), {
            status: 404,
            headers: { "content-type": "application/problem+json" },
          }),
      ),
    );
    try {
      renderViewer();
      await connect();
      await send(lines(2));
      await act(async () => {
        source().fail();
      });
      await act(async () => {
        vi.advanceTimersByTime(1000);
      });
      await act(async () => {
        (FakeEventSource.instances[1] as FakeEventSource).fail(true);
      });
      await act(async () => {});
      const note = "Logs were interrupted. Some lines may be missing.";
      expect(screen.getAllByText(note)).toHaveLength(1);
      expect(screen.getByRole("log").textContent).toContain("line 1");
      expect(screen.queryByText("Reconnecting…")).toBeNull();
      await act(async () => {
        vi.advanceTimersByTime(60_000);
      });
      expect(screen.getAllByText(note)).toHaveLength(1);
      expect(FakeEventSource.instances).toHaveLength(2);
    } finally {
      vi.useRealTimers();
    }
  });

  it("does not claim the access changed for a stream that simply could not be opened", async () => {
    renderViewer();
    await act(async () => {
      source().fail(true);
    });
    expect(screen.queryByText("Your access changed. Logs were closed.")).toBeNull();
    expect(screen.queryByText("Live")).toBeNull();
  });
});

describe("a first load that fails", () => {
  it("says Couldn't load logs. and offers Retry, which opens a new stream for the same viewer", async () => {
    renderViewer();
    const sid = new URL(source().url, "http://x").searchParams.get("sid");
    await act(async () => {
      source().fail(true);
    });
    expect(screen.getByText("Couldn't load logs.")).toBeTruthy();
    expect(screen.getByText("Check the connection and try again.")).toBeTruthy();
    expect(screen.getByRole("log").querySelectorAll("[data-stream]")).toHaveLength(0);
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(FakeEventSource.instances).toHaveLength(2);
    const again = FakeEventSource.instances[1] as FakeEventSource;
    expect(new URL(again.url, "http://x").searchParams.get("sid")).toBe(sid);
    expect(screen.queryByText("Couldn't load logs.")).toBeNull();
    await act(async () => {
      again.open();
      again.sendFrame("ready", { tty: false });
    });
    expect(screen.getByRole("log", { name: "Logs for web" })).toBeTruthy();
    expect(screen.getByText("Live")).toBeTruthy();
  });

  it("does not show the error once the stream has been live", async () => {
    renderViewer();
    await connect();
    await send(lines(1));
    await act(async () => {
      source().fail(true);
    });
    expect(screen.queryByText("Couldn't load logs.")).toBeNull();
    expect(screen.getByRole("log").textContent).toContain("line 0");
  });
});

describe("a container that stops and starts", () => {
  const clock = (iso: string) => {
    const d = new Date(iso);
    return `${String(d.getHours()).padStart(2, "0")}:${String(d.getMinutes()).padStart(2, "0")}`;
  };
  const state = async (what: string, at: string) => {
    await act(async () => {
      source().sendFrame("state", { state: what, at });
    });
  };

  it("adds the stop row when the container stops and keeps every line", async () => {
    renderViewer();
    await connect();
    await send(lines(3));
    await state("stopped", "2026-10-09T12:34:56.000Z");
    const surface = screen.getByRole("log");
    expect(surface.textContent).toContain("line 2");
    expect(
      screen.getByText(
        `Container stopped ${clock("2026-10-09T12:34:56.000Z")}. Waiting for it to start.`,
      ),
    ).toBeTruthy();
    expect(surface.querySelector('[data-marker="stopped"]')).not.toBeNull();
  });

  it("adds a divider when the container restarts, keeps the buffer and follows the new run", async () => {
    renderViewer();
    await connect();
    await send(lines(3));
    await state("stopped", "2026-10-09T12:34:56.000Z");
    await state("restarted", "2026-10-09T12:40:00.000Z");
    await send(lines(2, 3));
    const surface = screen.getByRole("log");
    expect(
      screen.getByText(`Container restarted ${clock("2026-10-09T12:40:00.000Z")}`),
    ).toBeTruthy();
    expect(surface.querySelector('[data-marker="restarted"]')).not.toBeNull();
    for (const n of [0, 1, 2, 3, 4]) expect(surface.textContent).toContain(`line ${n}`);
    // Five lines, two marker rows and the row above them; the view moved to the end.
    expect(surface.scrollTop).toBe(8 * LINE);
  });

  it("shows the stop row once when a reconnect is told the container is still stopped", async () => {
    renderViewer();
    await connect();
    await send(lines(1));
    await state("stopped", "2026-10-09T12:34:56.000Z");
    await state("stopped", "2026-10-09T12:35:30.000Z");
    expect(screen.getByRole("log").querySelectorAll('[data-marker="stopped"]')).toHaveLength(1);
  });
});

// ---- plan 04-20: range, timestamps, wrap, search, the pill, earlier lines ----

const surface = () => screen.getByRole("log");
const pressed = (name: string) => screen.getByRole("button", { name }).getAttribute("aria-pressed");
const clickToggle = (name: string) => fireEvent.click(screen.getByRole("button", { name }));
const local = (iso: string, utc = false) => {
  const d = new Date(iso);
  const p = (n: number, w = 2) => String(n).padStart(w, "0");
  return utc
    ? `${p(d.getUTCHours())}:${p(d.getUTCMinutes())}:${p(d.getUTCSeconds())}.${p(d.getUTCMilliseconds(), 3)}`
    : `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}.${p(d.getMilliseconds(), 3)}`;
};

describe("the Show range", () => {
  it("opens a time range with the time and a tail that holds it", () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2026-10-09T12:00:00.000Z"));
    try {
      renderViewer({ params: { since: "1h" }, onParamsChange: vi.fn() });
      const url = new URL(source().url, "http://x");
      expect(url.searchParams.get("since")).toBe("2026-10-09T11:00:00.000Z");
      expect(url.searchParams.get("tail")).toBe("50000");
      expect(url.searchParams.get("until")).toBeNull();
    } finally {
      vi.useRealTimers();
    }
  });

  it("opens a custom range with both ends", () => {
    const from = "2026-10-09T10:00:00.000Z";
    const to = "2026-10-09T11:00:00.000Z";
    renderViewer({ params: { since: `${from}..${to}` }, onParamsChange: vi.fn() });
    const url = new URL(source().url, "http://x");
    expect([url.searchParams.get("since"), url.searchParams.get("until")]).toEqual([from, to]);
  });

  it("opens all of the log with tail=all", () => {
    renderViewer({ params: { since: "all" }, onParamsChange: vi.fn() });
    expect(new URL(source().url, "http://x").searchParams.get("tail")).toBe("all");
  });

  it("reopens the stream with an empty buffer when the range changes", async () => {
    const { rerender } = renderViewer({ params: {}, onParamsChange: vi.fn() });
    await connect();
    await send(lines(3));
    expect(surface().textContent).toContain("line 2");
    rerender(
      <LogViewer
        envId="e1"
        containerId="web-id"
        nodeId="n1"
        name="web"
        params={{ since: "5m" }}
        onParamsChange={vi.fn()}
      />,
    );
    expect(FakeEventSource.instances).toHaveLength(2);
    expect(FakeEventSource.instances[0]?.closed).toBe(true);
    expect(surface().textContent).not.toContain("line 2");
  });

  it("writes the choice into the address", () => {
    const onParamsChange = vi.fn();
    renderViewer({ params: {}, onParamsChange });
    fireEvent.keyDown(screen.getByRole("combobox", { name: "Show" }), { key: "Enter" });
    fireEvent.click(screen.getByRole("option", { name: "Last 24 hours" }));
    expect(onParamsChange).toHaveBeenCalledWith({ since: "24h" });
  });
});

describe("timestamps", () => {
  const A = "2026-10-09T12:58:01.123456789Z";

  it("are off until asked for, and then show HH:mm:ss.SSS in the browser's time zone", async () => {
    renderViewer();
    await connect();
    await send([{ t: A, s: "o", m: "first" }]);
    expect(surface().querySelector("time")).toBeNull();
    clickToggle("Timestamps");
    expect(pressed("Timestamps")).toBe("true");
    const cell = surface().querySelector("time");
    expect(cell?.textContent).toBe(local(A));
    expect(cell?.textContent).toMatch(/^\d\d:\d\d:\d\d\.\d{3}$/);
    expect(cell?.getAttribute("title")).toBe("Time");
  });

  it("switch to UTC, and the column says so", async () => {
    renderViewer();
    await connect();
    await send([{ t: A, s: "o", m: "first" }]);
    clickToggle("Timestamps");
    clickToggle("UTC");
    expect(surface().querySelector("time")?.textContent).toBe("12:58:01.123");
    expect(surface().querySelector("time")?.getAttribute("title")).toBe("Time (UTC)");
    expect(pressed("UTC")).toBe("true");
  });

  it("put a divider with the date where the day changes", async () => {
    renderViewer();
    await connect();
    await send([
      { t: "2026-10-08T12:00:00.000000000Z", s: "o", m: "yesterday" },
      { t: "2026-10-08T12:00:01.000000000Z", s: "o", m: "yesterday again" },
      { t: "2026-10-09T12:00:00.000000000Z", s: "o", m: "today" },
    ]);
    clickToggle("Timestamps");
    clickToggle("UTC");
    const dividers = surface().querySelectorAll("[data-day-divider]");
    // One above the first day and one between the days.
    expect(dividers).toHaveLength(2);
    const label = (iso: string) =>
      new Date(iso).toLocaleDateString(undefined, { dateStyle: "medium", timeZone: "UTC" });
    expect(dividers[0]?.textContent).toBe(label("2026-10-08T12:00:00Z"));
    expect(dividers[1]?.textContent).toBe(label("2026-10-09T12:00:00Z"));
    clickToggle("Timestamps");
    expect(surface().querySelectorAll("[data-day-divider]")).toHaveLength(0);
  });

  it("are remembered by this browser, and the address can override them", async () => {
    const first = renderViewer();
    await connect();
    clickToggle("Timestamps");
    clickToggle("Wrap");
    expect(JSON.parse(localStorage.getItem("coaming.logs.view") ?? "{}")).toEqual({
      wrap: true,
      timestamps: true,
      utc: false,
    });
    first.unmount();

    FakeEventSource.reset();
    renderViewer();
    expect(pressed("Timestamps")).toBe("true");
    expect(pressed("Wrap")).toBe("true");
    cleanup();

    FakeEventSource.reset();
    renderViewer({ params: { ts: false, wrap: false }, onParamsChange: vi.fn() });
    expect(pressed("Timestamps")).toBe("false");
    expect(pressed("Wrap")).toBe("false");
  });

  it("are written into the address with wrap and UTC", () => {
    const onParamsChange = vi.fn();
    renderViewer({ params: {}, onParamsChange });
    clickToggle("Timestamps");
    expect(onParamsChange).toHaveBeenLastCalledWith({ ts: true, wrap: false, utc: false });
  });
});

describe("wrap", () => {
  it("wraps long lines when on and scrolls the surface as one when off", async () => {
    renderViewer();
    await connect();
    await send([{ t: "2026-10-09T12:00:00.000000000Z", s: "o", m: "x".repeat(400) }]);
    const row = () => surface().querySelector("[data-stream]") as HTMLElement;
    expect(row().className).toContain("whitespace-pre");
    expect(row().className).not.toContain("whitespace-pre-wrap");
    expect(row().className).toContain("w-max");
    clickToggle("Wrap");
    expect(row().className).toContain("whitespace-pre-wrap");
    expect(row().className).not.toContain("w-max");
    // Never shortened.
    expect(row().textContent).toBe("x".repeat(400));
  });
});

describe("search", () => {
  const log = [
    { t: "2026-10-09T12:00:00.000000000Z", s: "o" as const, m: "starting up" },
    { t: "2026-10-09T12:00:01.000000000Z", s: "e" as const, m: "ERROR: disk full" },
    { t: "2026-10-09T12:00:02.000000000Z", s: "o" as const, m: "all good" },
    { t: "2026-10-09T12:00:03.000000000Z", s: "o" as const, m: "error again, and another error" },
  ];
  const type = (value: string) =>
    fireEvent.change(screen.getByRole("searchbox"), { target: { value } });

  async function typed(value: string) {
    vi.useFakeTimers();
    try {
      type(value);
      await act(async () => {
        await vi.advanceTimersByTimeAsync(200);
      });
    } finally {
      vi.useRealTimers();
    }
  }

  it("highlights the matches in the loaded lines and counts them", async () => {
    renderViewer();
    await connect();
    await send(log);
    await typed("error");
    const marks = surface().querySelectorAll("mark");
    expect(Array.from(marks).map((m) => m.textContent)).toEqual(["ERROR", "error", "error"]);
    // The line the counter is on is inverted as well as marked.
    expect(marks[0]?.getAttribute("data-match")).toBe("current");
    expect(marks[2]?.getAttribute("data-match")).toBe("match");
    expect(screen.getByText("1 of 2")).toBeTruthy();
    expect(surface().textContent).toContain("starting up");
  });

  it("moves to the next match and the one before, round the ends", async () => {
    renderViewer();
    await connect();
    await send(log);
    await typed("error");
    fireEvent.click(screen.getByRole("button", { name: "Next match" }));
    expect(screen.getByText("2 of 2")).toBeTruthy();
    expect(surface().querySelectorAll("mark")[2]?.getAttribute("data-match")).toBe("current");
    fireEvent.click(screen.getByRole("button", { name: "Next match" }));
    expect(screen.getByText("1 of 2")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Previous match" }));
    expect(screen.getByText("2 of 2")).toBeTruthy();
  });

  it("can show only the lines that match", async () => {
    renderViewer();
    await connect();
    await send(log);
    await typed("error");
    clickToggle("Only matching lines");
    const texts = Array.from(surface().querySelectorAll("[data-stream]")).map((r) => r.textContent);
    expect(texts).toHaveLength(2);
    expect(texts[0]).toContain("ERROR: disk full");
    clickToggle("Only matching lines");
    expect(surface().querySelectorAll("[data-stream]")).toHaveLength(4);
  });

  it("honours Match case and Use regular expression", async () => {
    renderViewer();
    await connect();
    await send(log);
    await typed("ERROR");
    clickToggle("Match case");
    await act(async () => {});
    expect(Array.from(surface().querySelectorAll("mark")).map((m) => m.textContent)).toEqual([
      "ERROR",
    ]);
    clickToggle("Match case");
    clickToggle("Use regular expression");
    await typed("err.r: d\\w+");
    expect(Array.from(surface().querySelectorAll("mark")).map((m) => m.textContent)).toEqual([
      "ERROR: disk",
    ]);
  });

  it("says an invalid pattern is not valid, and keeps showing the log", async () => {
    renderViewer();
    await connect();
    await send(log);
    clickToggle("Use regular expression");
    await typed("(");
    expect(screen.getByText("That isn't a valid pattern.")).toBeTruthy();
    expect(surface().querySelectorAll("mark")).toHaveLength(0);
    expect(surface().textContent).toContain("starting up");
  });

  it("says No matches, and never asks the server", async () => {
    const fetchSpy = vi.fn();
    vi.stubGlobal("fetch", fetchSpy);
    renderViewer();
    await connect();
    await send(log);
    await typed("zzz");
    expect(screen.getByText("No matches")).toBeTruthy();
    expect(fetchSpy).not.toHaveBeenCalled();
    expect(FakeEventSource.instances).toHaveLength(1);
  });

  it("searches the text without its colour codes", async () => {
    renderViewer();
    await connect();
    await send([
      { t: "2026-10-09T12:00:00.000000000Z", s: "o", m: "\u001b[31mred\u001b[0m alert" },
    ]);
    await typed("red alert");
    // The match spans the two colours, so it is drawn in two pieces that read as one.
    expect(Array.from(surface().querySelectorAll("mark")).map((m) => m.textContent)).toEqual([
      "red",
      " alert",
    ]);
  });

  it("keeps the query in the address once typing has stopped, and takes one from it", async () => {
    const onParamsChange = vi.fn();
    const { rerender } = renderViewer({ params: {}, onParamsChange });
    await connect();
    vi.useFakeTimers();
    try {
      type("er");
      await act(async () => {
        await vi.advanceTimersByTimeAsync(300);
      });
      // Still typing, as far as the address can tell.
      expect(onParamsChange).not.toHaveBeenCalled();
      await act(async () => {
        await vi.advanceTimersByTimeAsync(400);
      });
    } finally {
      vi.useRealTimers();
    }
    expect(onParamsChange).toHaveBeenLastCalledWith({ q: "er" });
    rerender(
      <LogViewer
        envId="e1"
        containerId="web-id"
        nodeId="n1"
        name="web"
        params={{ q: "from the link" }}
        onParamsChange={onParamsChange}
      />,
    );
    expect((screen.getByRole("searchbox") as HTMLInputElement).value).toBe("from the link");
  });

  it("starts from the query of the address", () => {
    renderViewer({ params: { q: "boom" }, onParamsChange: vi.fn() });
    expect((screen.getByRole("searchbox") as HTMLInputElement).value).toBe("boom");
  });
});

describe("pausing and the pill", () => {
  async function scrollUp() {
    const el = surface();
    Object.defineProperty(el, "scrollHeight", { configurable: true, value: 101 * LINE });
    Object.defineProperty(el, "clientHeight", { configurable: true, value: VIEWPORT });
    el.scrollTop = 200;
    fireEvent.scroll(el);
    return el;
  }

  it("shows no pill while following", async () => {
    renderViewer();
    await connect();
    await send(lines(100));
    expect(screen.queryByRole("button", { name: /Jump to latest/ })).toBeNull();
  });

  it("pauses when the viewer scrolls up, says so once, and counts the lines that come", async () => {
    renderViewer();
    await connect();
    await send(lines(100));
    const el = await scrollUp();
    expect(screen.getAllByRole("status").some((s) => s.textContent === "Auto-scroll paused")).toBe(
      true,
    );
    expect(screen.getByRole("button", { name: "Jump to latest" })).toBeTruthy();
    await send(lines(3, 100));
    expect(el.scrollTop).toBe(200);
    expect(screen.getByRole("button", { name: "Jump to latest (3 new)" })).toBeTruthy();
    await send(lines(1, 103));
    expect(screen.getByRole("button", { name: "Jump to latest (4 new)" })).toBeTruthy();
    expect(screen.getAllByText("Auto-scroll paused")).toHaveLength(1);
  });

  it("says the pill's count in the singular for one line", async () => {
    renderViewer();
    await connect();
    await send(lines(100));
    await scrollUp();
    await send(lines(1, 100));
    expect(screen.getByRole("button", { name: "Jump to latest (1 new)" })).toBeTruthy();
  });

  it("resumes when the pill is pressed", async () => {
    renderViewer();
    await connect();
    await send(lines(100));
    const el = await scrollUp();
    await send(lines(5, 100));
    fireEvent.click(screen.getByRole("button", { name: /Jump to latest/ }));
    expect(screen.queryByRole("button", { name: /Jump to latest/ })).toBeNull();
    expect(el.scrollTop).toBe((105 + 1) * LINE);
    await send(lines(2, 105));
    expect(el.scrollTop).toBe((107 + 1) * LINE);
  });

  it("resumes on End, and when scrolled back to the bottom", async () => {
    renderViewer();
    await connect();
    await send(lines(100));
    let el = await scrollUp();
    fireEvent.keyDown(el, { key: "End" });
    expect(screen.queryByRole("button", { name: /Jump to latest/ })).toBeNull();

    el = await scrollUp();
    expect(screen.getByRole("button", { name: /Jump to latest/ })).toBeTruthy();
    el.scrollTop = 101 * LINE - VIEWPORT;
    fireEvent.scroll(el);
    expect(screen.queryByRole("button", { name: /Jump to latest/ })).toBeNull();
  });
});

describe("earlier lines", () => {
  function page(pageLines: LogLineData[], startOfLogs = false) {
    const fetchMock = vi.fn(
      async (_req: Request) =>
        new Response(JSON.stringify({ lines: pageLines, startOfLogs }), {
          status: 200,
          headers: { "content-type": "application/json" },
        }),
    );
    vi.stubGlobal("fetch", fetchMock);
    return fetchMock;
  }
  const stamp = (n: number) => `2026-10-09T12:00:${String(n).padStart(2, "0")}.000000000Z`;
  const text = () =>
    Array.from(surface().querySelectorAll("[data-stream]")).map((r) => r.textContent);

  it("offers to load earlier lines above the first", async () => {
    renderViewer();
    await connect();
    await send(lines(3));
    expect(screen.getByRole("button", { name: "Load earlier lines" })).toBeTruthy();
  });

  it("asks for the page before the oldest line, one nanosecond late, for the same viewer", async () => {
    const fetchMock = page([{ t: stamp(0), s: "o", m: "older" }]);
    renderViewer();
    await connect();
    await send([{ t: "2026-10-09T12:00:10.000000000Z", s: "o", m: "held" }]);
    fireEvent.click(screen.getByRole("button", { name: "Load earlier lines" }));
    await act(async () => {});
    const req = fetchMock.mock.calls[0]?.[0] as Request;
    const url = new URL(req.url);
    expect(url.pathname).toBe("/api/env/e1/containers/web-id/logs");
    expect(url.searchParams.get("until")).toBe("2026-10-09T12:00:10.000000001Z");
    expect(url.searchParams.get("tail")).toBe("500");
    expect(url.searchParams.get("nodeId")).toBe("n1");
    expect(url.searchParams.get("sid")).toBe(
      new URL(source().url, "http://x").searchParams.get("sid"),
    );
  });

  it("puts the page above the lines, without repeating the lines held at the seam", async () => {
    // The engine answers up to a nanosecond past the oldest line held, so the page ends
    // with the lines at that stamp, the held ones included, and one held line is
    // written twice at the same instant.
    page([
      { t: stamp(0), s: "o", m: "older 1" },
      { t: stamp(1), s: "o", m: "older 2" },
      { t: stamp(5), s: "o", m: "same stamp, not held" },
      { t: stamp(5), s: "o", m: "tick" },
    ]);
    renderViewer();
    await connect();
    await send([
      { t: stamp(5), s: "o", m: "tick" },
      { t: stamp(5), s: "o", m: "tick" },
      { t: stamp(6), s: "o", m: "newer" },
    ]);
    fireEvent.click(screen.getByRole("button", { name: "Load earlier lines" }));
    await act(async () => {});
    // "tick" is held twice and the page has it once: no more are added; the line that
    // shares the stamp but is not held is kept.
    expect(text()).toEqual(["older 1", "older 2", "same stamp, not held", "tick", "tick", "newer"]);
  });

  it("says Start of logs when there is nothing earlier, and offers nothing more", async () => {
    page([{ t: stamp(0), s: "o", m: "first line ever" }], true);
    renderViewer();
    await connect();
    await send([{ t: stamp(5), s: "o", m: "held" }]);
    fireEvent.click(screen.getByRole("button", { name: "Load earlier lines" }));
    await act(async () => {});
    expect(screen.getByText("Start of logs")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Load earlier lines" })).toBeNull();
    expect(text()[0]).toBe("first line ever");
  });

  it("treats a page that adds nothing as the start, so it cannot be asked for again and again", async () => {
    page([{ t: stamp(5), s: "o", m: "held" }]);
    renderViewer();
    await connect();
    await send([{ t: stamp(5), s: "o", m: "held" }]);
    fireEvent.click(screen.getByRole("button", { name: "Load earlier lines" }));
    await act(async () => {});
    expect(screen.getByText("Start of logs")).toBeTruthy();
  });

  it("keeps the viewer's place when lines arrive above it", async () => {
    page(Array.from({ length: 10 }, (_, i) => ({ t: stamp(i), s: "o" as const, m: `older ${i}` })));
    renderViewer();
    await connect();
    await send(lines(100, 20));
    const el = surface();
    Object.defineProperty(el, "scrollHeight", { configurable: true, value: 101 * LINE });
    Object.defineProperty(el, "clientHeight", { configurable: true, value: VIEWPORT });
    el.scrollTop = 0;
    fireEvent.scroll(el);
    fireEvent.click(screen.getByRole("button", { name: "Load earlier lines" }));
    await act(async () => {});
    expect(el.scrollTop).toBe(10 * LINE);
  });

  it("says it could not load, and tries again on Retry", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(
        async () =>
          new Response("{}", { status: 503, headers: { "content-type": "application/json" } }),
      ),
    );
    renderViewer();
    await connect();
    await send([{ t: stamp(5), s: "o", m: "held" }]);
    fireEvent.click(screen.getByRole("button", { name: "Load earlier lines" }));
    await act(async () => {});
    expect(screen.getByText(/Couldn't load logs\./)).toBeTruthy();
    page([{ t: stamp(0), s: "o", m: "back" }], true);
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await act(async () => {});
    expect(text()[0]).toBe("back");
  });
});

describe("the buffer cap", () => {
  it("says older lines were dropped when more than 50 000 arrive, and offers no Load earlier", async () => {
    renderViewer();
    await connect();
    for (let i = 0; i < 6; i++) await send(lines(10_000, i * 10_000));
    expect(
      screen.getByText(
        "Older lines were dropped to keep this fast. Download the logs to get everything.",
      ),
    ).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Load earlier lines" })).toBeNull();
    expect(surface().textContent).not.toContain("line 0 ");
    // Sixty thousand lines in, only the ones in view are drawn.
    expect(surface().querySelectorAll("[data-stream]").length).toBeLessThan(200);
  });
});

describe("colour", () => {
  const ESC = "\u001b";

  it("draws colour codes as styled text through custom properties, never as markup", async () => {
    renderViewer();
    await connect();
    await send([
      {
        t: "2026-10-09T12:00:00.000000000Z",
        s: "o",
        m: `${ESC}[31mred${ESC}[0m plain ${ESC}[1mbold${ESC}[0m ${ESC}]8;;https://evil.test${"\u0007"}link${ESC}]8;;${"\u0007"} <b>x</b>`,
      },
    ]);
    const row = surface().querySelector("[data-stream]") as HTMLElement;
    expect(row.textContent).toBe("red plain bold link <b>x</b>");
    const red = within(row).getByText("red");
    expect(red.style.getPropertyValue("--log-fg")).toBe("var(--ansi-1)");
    expect(red.style.color).toBe("var(--log-fg)");
    expect(within(row).getByText("bold").className).toContain("font-semibold");
    expect(row.querySelector("a")).toBeNull();
    expect(row.querySelector("b")).toBeNull();
    expect(row.innerHTML).not.toContain(ESC);
    expect(row.innerHTML).not.toContain("evil.test");
  });

  it("leaves a colour too faint for the surface out, so the text stays readable", async () => {
    renderViewer();
    await connect();
    await send([
      { t: "2026-10-09T12:00:00.000000000Z", s: "o", m: `${ESC}[38;2;250;250;250mfaint${ESC}[0m` },
    ]);
    expect(within(surface()).getByText("faint").getAttribute("style")).toBeNull();
  });
});

describe("a log Docker cannot read back", () => {
  async function unreadable(options: Array<{ name: string; value?: string; masked: boolean }>) {
    render(
      <TooltipProvider>
        <LogViewer envId="e1" containerId="web-id" nodeId="n1" name="web" />
      </TooltipProvider>,
    );
    await act(async () => {
      source().open();
      source().sendFrame("unreadable", { driver: "none", options });
    });
  }

  it("shows the notice naming the driver in place of the viewer and the toolbar, and closes the stream", async () => {
    await unreadable([{ name: "tag", masked: true }]);
    expect(screen.getByText("Docker can't read these logs back")).toBeTruthy();
    expect(
      screen.getByText("This container sends its logs to none, so Docker has none to show."),
    ).toBeTruthy();
    expect(screen.queryByRole("log")).toBeNull();
    expect(screen.queryByRole("toolbar")).toBeNull();
    expect(source().closed).toBe(true);
    // Information, not an error.
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("lists the driver's options, with a value hidden unless the server sent it", async () => {
    await unreadable([
      { name: "syslog-address", masked: true },
      { name: "tag", value: "web", masked: false },
    ]);
    expect(screen.getByText("Driver options")).toBeTruthy();
    expect(screen.getByText("syslog-address")).toBeTruthy();
    expect(screen.getByText("tag")).toBeTruthy();
    expect(document.body.textContent).not.toContain("web");
    // The holder of secrets.reveal can show a value they were sent.
    const show = screen.getAllByRole("button", { name: /Show value/ });
    expect(show).toHaveLength(1);
    fireEvent.click(show[0] as HTMLElement);
    expect(screen.getByText("web")).toBeTruthy();
  });

  it("says nothing of options when there are none", async () => {
    await unreadable([]);
    expect(screen.queryByText("Driver options")).toBeNull();
  });
});

describe("downloading", () => {
  it("offers Download… in the toolbar, opening on the viewer's range and Timestamps choice", async () => {
    renderViewer({ params: { since: "1h", ts: true }, onParamsChange: vi.fn() });
    await connect();
    fireEvent.click(screen.getByRole("button", { name: "Download…" }));
    const from = screen.getByLabelText("From") as HTMLInputElement;
    expect(from.value).not.toBe("");
    expect(
      screen.getByRole("checkbox", { name: "Include timestamps" }).getAttribute("aria-checked"),
    ).toBe("true");
  });

  it("puts Download… in the More menu under 768px, opening the same form as a dialog", async () => {
    const width = window.innerWidth;
    window.innerWidth = 500;
    try {
      renderViewer();
      await connect();
      expect(screen.queryByRole("button", { name: "Download…" })).toBeNull();
      fireEvent.keyDown(screen.getByRole("button", { name: "More" }), { key: "Enter" });
      fireEvent.click(await screen.findByRole("menuitem", { name: "Download…" }));
      expect(await screen.findByRole("dialog", { name: "Download logs" })).toBeTruthy();
    } finally {
      window.innerWidth = width;
    }
  });
});

describe("the pop-out", () => {
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("builds the address of the pop-out with the settings carried", () => {
    const url = new URL(
      popoutLogsUrl(
        "e 1",
        { kind: "container", id: "web/id", nodeId: "n1" },
        {
          since: "1h",
          q: "404",
          ts: true,
          utc: false,
          wrap: true,
        },
      ),
      "http://x",
    );
    expect(url.pathname).toBe("/popout/e%201/containers/web%2Fid/logs");
    expect(url.searchParams.get("node")).toBe("n1");
    expect(url.searchParams.get("since")).toBe("1h");
    expect(url.searchParams.get("ts")).toBe("true");
    expect(url.searchParams.get("utc")).toBe("false");
    expect(url.searchParams.get("wrap")).toBe("true");
    // A query that reads as a number or a boolean stays text through the router.
    expect(url.searchParams.get("q")).toBe('"404"');
  });

  it("leaves out what is not set", () => {
    const url = new URL(popoutLogsUrl("e1", { kind: "container", id: "c" }, {}), "http://x");
    expect(url.search).toBe("");
  });

  it("opens the pop-out in a new window carrying the range, the search and the toggles", async () => {
    const open = vi.spyOn(window, "open").mockImplementation(() => null);
    renderViewer({
      params: { since: "5m", q: "boom", ts: true, utc: true, wrap: true },
      onParamsChange: vi.fn(),
    });
    await connect();
    fireEvent.click(screen.getByRole("button", { name: "Open in new window" }));
    expect(open).toHaveBeenCalledTimes(1);
    const [href, target, features] = open.mock.calls[0] as [string, string, string];
    const url = new URL(href, "http://x");
    expect(url.pathname).toBe("/popout/e1/containers/web-id/logs");
    expect(url.searchParams.get("node")).toBe("n1");
    expect(url.searchParams.get("since")).toBe("5m");
    expect(url.searchParams.get("q")).toBe("boom");
    expect(url.searchParams.get("ts")).toBe("true");
    expect(url.searchParams.get("utc")).toBe("true");
    expect(url.searchParams.get("wrap")).toBe("true");
    expect(target).toBe("_blank");
    expect(features).toContain("noopener");
  });

  it("carries the choices this browser remembers when the address names none", async () => {
    localStorage.setItem(
      "coaming.logs.view",
      JSON.stringify({ wrap: true, timestamps: true, utc: false }),
    );
    const open = vi.spyOn(window, "open").mockImplementation(() => null);
    renderViewer();
    await connect();
    fireEvent.click(screen.getByRole("button", { name: "Open in new window" }));
    const url = new URL((open.mock.calls[0] as [string])[0], "http://x");
    expect(url.searchParams.get("ts")).toBe("true");
    expect(url.searchParams.get("wrap")).toBe("true");
  });

  it("is not offered inside the pop-out itself", async () => {
    renderViewer({ popout: true });
    await connect();
    expect(screen.queryByRole("button", { name: "Open in new window" })).toBeNull();
  });
});

describe("a hidden pop-out", () => {
  const hide = (hidden: boolean) => {
    Object.defineProperty(document, "hidden", { configurable: true, get: () => hidden });
    document.dispatchEvent(new Event("visibilitychange"));
  };
  afterEach(() => {
    Reflect.deleteProperty(document, "hidden");
  });

  it("closes its stream after a minute hidden and reopens it from the last line when seen again", async () => {
    vi.useFakeTimers();
    try {
      renderViewer({ popout: true });
      await connect();
      await send(lines(2));
      const first = source();
      await act(async () => {
        hide(true);
        vi.advanceTimersByTime(59_000);
      });
      expect(first.closed).toBe(false);
      await act(async () => {
        vi.advanceTimersByTime(1000);
      });
      expect(first.closed).toBe(true);
      expect(FakeEventSource.instances).toHaveLength(1);
      await act(async () => {
        hide(false);
      });
      expect(FakeEventSource.instances).toHaveLength(2);
      const again = FakeEventSource.instances[1] as FakeEventSource;
      expect(new URL(again.url, "http://x").searchParams.get("after")).toBe(lines(2)[1]?.t);
    } finally {
      vi.useRealTimers();
    }
  });

  it("keeps its stream when it is seen again within the minute", async () => {
    vi.useFakeTimers();
    try {
      renderViewer({ popout: true });
      await connect();
      await act(async () => {
        hide(true);
        vi.advanceTimersByTime(30_000);
        hide(false);
        vi.advanceTimersByTime(60_000);
      });
      expect(source().closed).toBe(false);
      expect(FakeEventSource.instances).toHaveLength(1);
    } finally {
      vi.useRealTimers();
    }
  });

  it("leaves the Logs tab alone: it keeps its stream hidden or not", async () => {
    vi.useFakeTimers();
    try {
      renderViewer();
      await connect();
      await act(async () => {
        hide(true);
        vi.advanceTimersByTime(120_000);
      });
      expect(source().closed).toBe(false);
    } finally {
      vi.useRealTimers();
    }
  });
});

// A service's log in the same viewer (plan 06-29, OBS-02, D-32, D-35): the source is the
// service, every line names its task in a coloured prefix, and the Task names toggle takes
// the prefixes out of the page.
describe("a service's log", () => {
  const tasks = [
    { id: "t1", slot: 1, node: "alpha", state: "running" },
    { id: "t2", slot: 2, node: "beta", state: "running" },
    { id: "t3", slot: 3, node: "alpha", state: "running" },
  ];

  function svcLine(
    i: number,
    task: string,
    slot: number,
    node: string,
    m = `msg ${i}`,
  ): LogLineData {
    return {
      t: `2026-10-14T10:00:${String(i).padStart(2, "0")}.000000000Z`,
      s: "o",
      m,
      task,
      slot,
      node,
    };
  }

  function renderService(props: Partial<ComponentProps<typeof LogViewer>> = {}) {
    return render(
      <TooltipProvider>
        <LogViewer envId="e1" source={{ kind: "service", id: "web" }} name="web" {...props} />
      </TooltipProvider>,
    );
  }

  async function connectService(list: unknown[] = tasks) {
    await act(async () => {
      source().open();
      source().sendFrame("ready", { tty: false, tasks: list });
    });
  }

  it("opens the service's stream on the last 500 lines, with the filters from the address", () => {
    renderService({ params: { task: "t1,t2", node: "alpha", stopped: true } });
    const url = new URL(source().url, "http://x");
    expect(url.pathname).toBe("/api/env/e1/services/web/logs/stream");
    expect(url.searchParams.get("tail")).toBe("500");
    expect(url.searchParams.get("sid")).toMatch(/^[A-Za-z0-9_-]{22}$/);
    expect(url.searchParams.get("task")).toBe("t1,t2");
    expect(url.searchParams.get("node")).toBe("alpha");
    expect(url.searchParams.get("stopped")).toBe("1");
    expect(url.searchParams.has("nodeId")).toBe(false);
  });

  it("puts a coloured prefix on every line: slot@node, the colour of the slot", async () => {
    renderService();
    await connectService();
    await send([
      svcLine(1, "t1", 1, "alpha"),
      svcLine(2, "t2", 2, "beta"),
      svcLine(3, "t1", 1, "alpha"),
    ]);
    const prefixes = screen.getAllByText(/^\d@/);
    expect(prefixes.map((p) => p.textContent)).toEqual(["1@alpha", "2@beta", "1@alpha"]);
    expect((prefixes[0] as HTMLElement).style.color).toBe("var(--ansi-4)");
    expect((prefixes[1] as HTMLElement).style.color).toBe("var(--ansi-5)");
    expect((prefixes[2] as HTMLElement).style.color).toBe("var(--ansi-4)");
    // A prefix is not part of what a copy takes.
    expect((prefixes[0] as HTMLElement).className).toContain("select-none");
    const row = screen.getByRole("log").querySelector("[data-stream]");
    expect(row?.textContent).toContain("msg 1");
  });

  it("names a task of a global service by the service and colours it by its node", async () => {
    renderService();
    await connectService();
    await send([
      svcLine(1, "g1", 0, "alpha"),
      svcLine(2, "g2", 0, "beta"),
      svcLine(3, "g3", 0, "alpha"),
    ]);
    const prefixes = screen.getAllByText(/^web@/);
    expect(prefixes.map((p) => p.textContent)).toEqual(["web@alpha", "web@beta", "web@alpha"]);
    expect((prefixes[0] as HTMLElement).style.color).toBe("var(--ansi-4)");
    expect((prefixes[1] as HTMLElement).style.color).toBe("var(--ansi-5)");
    expect((prefixes[2] as HTMLElement).style.color).toBe("var(--ansi-4)");
  });

  it("keeps the service name in the prefix where the header is not on screen", async () => {
    renderService({ showServiceName: true });
    await connectService();
    await send([svcLine(1, "t2", 2, "beta")]);
    expect(screen.getByText("web.2@beta")).toBeTruthy();
  });

  it("takes the prefixes out of the page with Task names, remembers it, and says so in the address", async () => {
    const onParamsChange = vi.fn();
    renderService({ params: {}, onParamsChange });
    await connectService();
    await send([svcLine(1, "t1", 1, "alpha")]);
    const toggle = screen.getByRole("button", { name: "Task names" });
    expect(toggle.getAttribute("aria-pressed")).toBe("true");
    expect(screen.getByText("1@alpha")).toBeTruthy();

    await act(async () => {
      fireEvent.click(toggle);
    });
    expect(onParamsChange).toHaveBeenCalledWith(expect.objectContaining({ prefix: false }));
    expect(JSON.parse(localStorage.getItem("coaming.logs.view") ?? "{}").taskNames).toBe(false);
  });

  it("hides the prefixes from the DOM when the address says prefix is off", async () => {
    renderService({ params: { prefix: false } });
    await connectService();
    await send([svcLine(1, "t1", 1, "alpha")]);
    expect(screen.queryByText("1@alpha")).toBeNull();
    expect(screen.getByRole("log").textContent).toContain("msg 1");
    expect(screen.getByRole("button", { name: "Task names" }).getAttribute("aria-pressed")).toBe(
      "false",
    );
  });

  it("starts with the prefixes off when this browser remembers that", async () => {
    localStorage.setItem("coaming.logs.view", JSON.stringify({ taskNames: false }));
    renderService();
    await connectService();
    await send([svcLine(1, "t1", 1, "alpha")]);
    expect(screen.queryByText("1@alpha")).toBeNull();
  });

  it("makes the prefix gutter as wide as the longest loaded prefix, from 12 to 28 characters", async () => {
    renderService();
    await connectService();
    await send([svcLine(1, "t1", 1, "alpha")]);
    expect((screen.getByText("1@alpha") as HTMLElement).style.width).toBe("12ch");
    const long = "a-node-with-a-very-long-host-name-indeed";
    await send([svcLine(2, "t2", 2, long)]);
    const wide = screen.getByText(`2@${long}`) as HTMLElement;
    expect(wide.style.width).toBe("28ch");
    expect(wide.className).toContain("truncate");
    expect((screen.getByText("1@alpha") as HTMLElement).style.width).toBe("28ch");
  });

  it("says Following 3 tasks beside the Live chip, and follows the tasks event", async () => {
    renderService();
    expect(screen.queryByText(/Following/)).toBeNull();
    await connectService();
    expect(screen.getByText("Live")).toBeTruthy();
    expect(screen.getByText("Following 3 tasks")).toBeTruthy();
    await act(async () => {
      source().sendFrame("tasks", { tty: false, tasks: tasks.slice(0, 1) });
    });
    expect(screen.getByText("Following 1 task")).toBeTruthy();
  });

  it("says there is no log output yet, and what will change that", async () => {
    renderService();
    await connectService();
    expect(screen.getByText("No log output yet")).toBeTruthy();
    expect(
      screen.getByText("New lines appear here as the service's tasks write them."),
    ).toBeTruthy();
  });

  it("says the same for a service with no tasks", async () => {
    renderService();
    await connectService([]);
    expect(screen.getByText("No log output yet")).toBeTruthy();
    expect(screen.getByText("Following 0 tasks")).toBeTruthy();
  });

  it("says no task matches when filters leave none", async () => {
    renderService({ params: { task: "nope" } });
    await connectService([]);
    expect(screen.getByText("No tasks match these filters.")).toBeTruthy();
    expect(screen.queryByText("No log output yet")).toBeNull();
  });

  it("draws a service line as text, never as markup", async () => {
    renderService();
    await connectService();
    await send([svcLine(1, "t1", 1, "alpha", "<img src=x onerror=alert(1)>")]);
    expect(screen.getByRole("log").querySelector("img")).toBeNull();
    expect(screen.getByRole("log").textContent).toContain("<img src=x onerror=alert(1)>");
  });

  it("offers a download, a pop-out and earlier lines, as a container's log does", async () => {
    renderService();
    await connectService();
    await send([svcLine(1, "t1", 1, "alpha")]);
    expect(screen.getByRole("button", { name: "Download…" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Open in new window" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Load earlier lines" })).toBeTruthy();
  });

  it("asks the service's page route for earlier lines with the viewer, the filters and the stopped switch", async () => {
    const fetchMock = vi.fn(
      async (_req: Request) =>
        new Response(
          JSON.stringify({
            lines: [svcLine(0, "t2", 2, "beta", "older")],
            startOfLogs: false,
          }),
          { status: 200, headers: { "content-type": "application/json" } },
        ),
    );
    vi.stubGlobal("fetch", fetchMock);
    renderService({
      params: { task: "t1,t2", node: "alpha", stopped: true },
      onParamsChange: vi.fn(),
    });
    await connectService();
    await send([svcLine(5, "t1", 1, "alpha", "held")]);
    fireEvent.click(screen.getByRole("button", { name: "Load earlier lines" }));
    await act(async () => {});
    const url = new URL((fetchMock.mock.calls[0] as [Request])[0].url);
    expect(url.pathname).toBe("/api/env/e1/services/web/logs");
    expect(url.searchParams.get("until")).toBe("2026-10-14T10:00:05.000000001Z");
    expect(url.searchParams.get("tail")).toBe("500");
    expect(url.searchParams.get("task")).toBe("t1,t2");
    expect(url.searchParams.get("node")).toBe("alpha");
    expect(url.searchParams.get("stopped")).toBe("true");
    expect(url.searchParams.get("sid")).toBe(
      new URL(source().url, "http://x").searchParams.get("sid"),
    );
    const rows = Array.from(screen.getByRole("log").querySelectorAll("[data-stream]"));
    expect(rows.map((r) => r.textContent)).toEqual(["2@betaolder", "1@alphaheld"]);
  });

  it("opens the pop-out of the service with the range, the search, the toggles, the prefixes and the filters", async () => {
    const open = vi.spyOn(window, "open").mockImplementation(() => null);
    renderService({
      params: {
        since: "5m",
        q: "boom",
        ts: true,
        utc: false,
        wrap: true,
        prefix: false,
        task: "t1",
        node: "alpha",
        stopped: true,
      },
      onParamsChange: vi.fn(),
    });
    await connectService();
    fireEvent.click(screen.getByRole("button", { name: "Open in new window" }));
    const url = new URL((open.mock.calls[0] as [string])[0], "http://x");
    expect(url.pathname).toBe("/popout/e1/services/web/logs");
    expect(Object.fromEntries(url.searchParams)).toEqual({
      since: "5m",
      q: "boom",
      ts: "true",
      utc: "false",
      wrap: "true",
      prefix: "false",
      task: "t1",
      node: "alpha",
      stopped: "true",
    });
    open.mockRestore();
  });

  it("does not offer another pop-out inside the pop-out of a service", async () => {
    renderService({ popout: true });
    await connectService();
    expect(screen.queryByRole("button", { name: "Open in new window" })).toBeNull();
  });

  it("lets go of the stream of a pop-out hidden for a minute and picks it up from the last line", async () => {
    vi.useFakeTimers();
    const hide = (hidden: boolean) => {
      Object.defineProperty(document, "hidden", { configurable: true, get: () => hidden });
      document.dispatchEvent(new Event("visibilitychange"));
    };
    try {
      renderService({ popout: true });
      await connectService();
      await send([svcLine(1, "t1", 1, "alpha")]);
      const first = source();
      await act(async () => {
        hide(true);
        vi.advanceTimersByTime(60_000);
      });
      expect(first.closed).toBe(true);
      await act(async () => {
        hide(false);
      });
      const again = FakeEventSource.instances[1] as FakeEventSource;
      expect(new URL(again.url, "http://x").searchParams.get("after")).toBe(
        "2026-10-14T10:00:01.000000000Z",
      );
    } finally {
      Reflect.deleteProperty(document, "hidden");
      vi.useRealTimers();
    }
  });

  it("opens the download of the service in the toolbar, with the merge note and no timestamps choice", async () => {
    renderService();
    await connectService();
    fireEvent.click(screen.getByRole("button", { name: "Download…" }));
    expect(
      screen.getByText(
        "Lines from all of the service's tasks are merged in time order, each starting with its task name. Downloads come straight from Docker, so they aren't limited to what's loaded here. Times are in UTC.",
      ),
    ).toBeTruthy();
    expect(screen.queryByRole("checkbox", { name: "Include timestamps" })).toBeNull();
  });

  it("does not mistake the end of the stream for a stopped container", async () => {
    renderService();
    await connectService();
    await send([svcLine(1, "t1", 1, "alpha")]);
    await act(async () => {
      source().sendFrame("state", { state: "stopped", at: "2026-10-14T10:05:00.000Z" });
    });
    expect(screen.queryByText(/Container stopped/)).toBeNull();
  });
});

// Markers between a service's lines, and its task and node filters (plan 06-30, OBS-02, D-33,
// D-34, D-37).
describe("a service's markers and filters", () => {
  const tasks = [
    { id: "t1", slot: 1, node: "alpha", state: "running" },
    { id: "t2", slot: 2, node: "beta", state: "running" },
  ];
  const options: FilterOptions = {
    loaded: true,
    tasks: [
      { id: "t1", slot: 1, node: "alpha", state: "running", ended: false },
      { id: "t2", slot: 2, node: "beta", state: "running", ended: false },
    ],
    nodes: [
      { name: "alpha", role: "manager", tasks: 1 },
      { name: "beta", role: "worker", tasks: 1 },
    ],
  };
  const at = (n: number) => `2026-10-14T10:00:${String(n).padStart(2, "0")}.000000000Z`;
  const row = (n: number, task: string, slot: number, node: string, m: string): LogLineData => ({
    t: at(n),
    s: "o",
    m,
    task,
    slot,
    node,
  });

  function renderService(props: Partial<ComponentProps<typeof LogViewer>> = {}) {
    return render(
      <TooltipProvider>
        <LogViewer
          envId="e1"
          source={{ kind: "service", id: "web" }}
          name="web"
          filterOptions={options}
          {...props}
        />
      </TooltipProvider>,
    );
  }

  async function ready(index = 0, list: unknown[] = tasks) {
    await act(async () => {
      const s = FakeEventSource.instances[index];
      s?.open();
      s?.sendFrame("ready", { tty: false, tasks: list });
    });
  }

  async function frame(type: string, data: unknown, index = 0) {
    await act(async () => {
      FakeEventSource.instances[index]?.sendFrame(type, data);
    });
  }

  async function lines(batch: LogLineData[], index = 0) {
    await act(async () => {
      FakeEventSource.instances[index]?.sendFrame("log", { lines: batch }, batch.at(-1)?.t ?? "");
    });
  }

  const replaced = {
    kind: "replaced",
    slot: 1,
    old: "t1",
    oldState: "failed",
    oldNode: "alpha",
    exitCode: 1,
    new: "t3",
    node: "beta",
    at: at(2),
  };

  it("puts a replacement marker between the old task's last line and the new task's first", async () => {
    renderService();
    await ready();
    await lines([
      row(1, "t1", 1, "alpha", "old one"),
      row(2, "t1", 1, "alpha", "old two"),
      row(5, "t3", 1, "beta", "new one"),
    ]);
    await frame("marker", replaced);
    const marker = screen.getByRole("separator", {
      name: "— web.1 replaced: task t1 failed (exit 1) → task t3 starting on beta —",
    });
    const two = screen.getByText("old two");
    const next = screen.getByText("new one");
    expect(two.compareDocumentPosition(marker) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(marker.compareDocumentPosition(next) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("does not announce a marker: the surface stays aria-live off", async () => {
    renderService();
    await ready();
    await lines([row(1, "t1", 1, "alpha", "one")]);
    await frame("marker", replaced);
    const marker = screen.getByRole("separator");
    expect(marker.closest("[aria-live]")?.getAttribute("aria-live")).toBe("off");
  });

  it("says a slot is waiting for a task and then that it was replaced, in the same place", async () => {
    renderService();
    await ready();
    await lines([row(1, "t1", 1, "alpha", "one")]);
    await frame("marker", {
      kind: "stopped",
      slot: 1,
      old: "t1",
      oldState: "rejected",
      oldNode: "alpha",
      at: at(1),
    });
    expect(
      screen.getByRole("separator", {
        name: "— web.1 stopped: task t1 was rejected. Waiting for a new task. —",
      }),
    ).toBeTruthy();
    await frame("marker", { ...replaced, oldState: "rejected", exitCode: undefined });
    expect(screen.getAllByRole("separator")).toHaveLength(1);
    expect(screen.getByRole("separator").getAttribute("aria-label")).toContain("replaced");
  });

  it("tells that a node cannot be reached, and that it is back, as rows of the log", async () => {
    renderService();
    await ready();
    await lines([row(1, "t2", 2, "beta", "one")]);
    await frame("node", { kind: "down", node: "beta", at: at(3) });
    expect(
      screen.getByRole("separator", {
        name: "— beta is down, so new lines from its tasks can't reach Coaming. —",
      }),
    ).toBeTruthy();
    await frame("node", { kind: "back", node: "beta", at: at(4) });
    expect(screen.getByRole("separator", { name: "— beta is back. —" })).toBeTruthy();
    // A reconnect that tells the same thing again adds nothing.
    await frame("node", { kind: "back", node: "beta", at: at(5) });
    expect(screen.getAllByRole("separator")).toHaveLength(2);
  });

  it("does not search a marker as a line of the log", async () => {
    renderService();
    await ready();
    await lines([row(1, "t1", 1, "alpha", "one")]);
    await frame("marker", replaced);
    vi.useFakeTimers();
    try {
      fireEvent.change(screen.getByRole("searchbox", { name: "Search logs" }), {
        target: { value: "replaced" },
      });
      await act(async () => {
        await vi.advanceTimersByTimeAsync(200);
      });
    } finally {
      vi.useRealTimers();
    }
    expect(screen.getByText("No matches")).toBeTruthy();
  });

  it("shows the filter row for a service and none for a container", async () => {
    renderService();
    expect(screen.getByRole("button", { name: "Tasks" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Nodes" })).toBeTruthy();
    expect(screen.getByRole("switch", { name: "Include stopped tasks" })).toBeTruthy();
    cleanup();
    render(<LogViewer envId="e1" containerId="c1" name="web" />);
    expect(screen.queryByRole("switch", { name: "Include stopped tasks" })).toBeNull();
  });

  it("shows what the address filters by as chips, and a task that matches none as not found", () => {
    renderService({ params: { task: "t2,nope0000", node: "alpha" } });
    expect(screen.getByText("Task: web.2")).toBeTruthy();
    expect(screen.getByText("Task not found")).toBeTruthy();
    expect(screen.getByText("Node: alpha")).toBeTruthy();
  });

  it("writes a chosen task, node and the stopped switch to the address", async () => {
    const onParamsChange = vi.fn();
    renderService({ params: {}, onParamsChange });
    await ready();
    fireEvent.click(screen.getByRole("button", { name: "Tasks" }));
    fireEvent.click(within(screen.getByRole("listbox")).getAllByRole("option")[1] as HTMLElement);
    expect(onParamsChange).toHaveBeenLastCalledWith(expect.objectContaining({ task: "t2" }));
    cleanup();

    const again = vi.fn();
    renderService({ params: { task: "t2" }, onParamsChange: again });
    fireEvent.click(screen.getByRole("button", { name: "Remove task filter web.2" }));
    expect(again).toHaveBeenLastCalledWith(
      expect.not.objectContaining({ task: expect.anything() }),
    );
    fireEvent.click(screen.getByRole("switch", { name: "Include stopped tasks" }));
    expect(again).toHaveBeenLastCalledWith(expect.objectContaining({ stopped: true }));
  });

  it("narrows the lines already loaded at once and reopens the stream for the new set", async () => {
    renderService();
    await ready();
    await lines([row(1, "t1", 1, "alpha", "from one"), row(2, "t2", 2, "beta", "from two")]);
    fireEvent.click(screen.getByRole("button", { name: "Tasks" }));
    fireEvent.click(within(screen.getByRole("listbox")).getAllByRole("option")[1] as HTMLElement);

    expect(FakeEventSource.instances).toHaveLength(2);
    expect(FakeEventSource.instances[0]?.closed).toBe(true);
    const url = new URL(FakeEventSource.instances[1]?.url ?? "", "http://x");
    expect(url.searchParams.get("task")).toBe("t2");
    expect(url.searchParams.get("tail")).toBe("500");
    // Before the new stream answers, the lines of the other task are already gone and the
    // others are not replaced by a skeleton.
    expect(screen.queryByText("from one")).toBeNull();
    expect(screen.getByText("from two")).toBeTruthy();
    expect(screen.getByRole("log").querySelectorAll('[data-slot="skeleton"]')).toHaveLength(0);

    // The new stream's lines replace what was loaded, and the view stays at the end.
    await ready(1, tasks.slice(1));
    await lines([row(2, "t2", 2, "beta", "from two"), row(3, "t2", 2, "beta", "and more")], 1);
    expect(screen.getAllByText("from two")).toHaveLength(1);
    expect(screen.getByText("and more")).toBeTruthy();
    expect(screen.queryByRole("button", { name: /Jump to latest/ })).toBeNull();
  });

  it("reopens the stream with stopped=1 when stopped tasks are included", async () => {
    renderService();
    await ready();
    fireEvent.click(screen.getByRole("switch", { name: "Include stopped tasks" }));
    expect(FakeEventSource.instances).toHaveLength(2);
    expect(
      new URL(FakeEventSource.instances[1]?.url ?? "", "http://x").searchParams.get("stopped"),
    ).toBe("1");
  });

  it("counts the tasks still running, not the ones that ended, as the ones it follows", async () => {
    renderService();
    await ready(0, [...tasks, { id: "t0", slot: 1, node: "alpha", state: "failed" }]);
    expect(screen.getByText("Following 2 tasks")).toBeTruthy();
  });
});
