// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

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

function renderViewer() {
  return render(<LogViewer envId="e1" containerId="web-id" nodeId="n1" name="web" />);
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
    // The browser clamps this to the real end; the viewer asks for the whole height.
    expect(surface.scrollTop).toBe(100 * LINE);
    await send(lines(10, 100));
    expect(surface.scrollTop).toBe(110 * LINE);
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
    expect(surface.scrollTop).toBe(120 * LINE);
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
    // Five lines and two marker rows; the view moved to the end.
    expect(surface.scrollTop).toBe(7 * LINE);
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
