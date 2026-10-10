// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import type { ComponentProps } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { TooltipProvider } from "@/components/ui/tooltip";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import type { LogLineData } from "@/features/logs/log-buffer";
import { LogViewer } from "@/features/logs/log-viewer";
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

  it("shows Reconnecting while the browser retries a dropped stream and Live again after", async () => {
    renderViewer();
    await connect();
    await act(async () => {
      source().fail();
    });
    expect(screen.getByText("Reconnecting…")).toBeTruthy();
    await connect();
    expect(screen.getByText("Live")).toBeTruthy();
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
    expect(JSON.parse(localStorage.getItem("bayplan.logs.view") ?? "{}")).toEqual({
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
    vi.useFakeTimers({ shouldAdvanceTime: true });
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
    vi.useFakeTimers({ shouldAdvanceTime: true });
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
  }, 20_000);
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
