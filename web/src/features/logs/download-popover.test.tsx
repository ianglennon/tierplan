// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import {
  DOWNLOAD_SPINNER_MS,
  DownloadDialog,
  DownloadPopover,
  defaultRange,
  downloadUrl,
} from "@/features/logs/download-popover";
import { toLocalInput } from "@/features/logs/log-prefs";
import "@/i18n";

// The Download… popover (plan 04-21, OBS-01, D-18, UI-SPEC "Log viewer" Download).

const target = { envId: "env 1", containerId: "web-1", nodeId: "node-a" };

let clicked: HTMLAnchorElement[] = [];

beforeEach(() => {
  clicked = [];
  vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (
    this: HTMLAnchorElement,
  ) {
    clicked.push(this.cloneNode() as HTMLAnchorElement);
  });
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.useRealTimers();
});

const open = () => fireEvent.click(screen.getByRole("button", { name: "Download…" }));
const fromBox = () => screen.getByLabelText("From") as HTMLInputElement;
const toBox = () => screen.getByLabelText("To") as HTMLInputElement;
const submit = () => screen.getByRole("button", { name: /Download logs$/ }) as HTMLButtonElement;

describe("downloadUrl", () => {
  it("is the download route with only what was chosen, under the base path", () => {
    expect(downloadUrl({ envId: "e", containerId: "c" }, {}, false)).toBe(
      "/api/env/e/containers/c/logs/download",
    );
    expect(
      downloadUrl(
        target,
        { since: "2026-10-09T10:00:00.000Z", until: "2026-10-09T11:00:00.000Z" },
        true,
      ),
    ).toBe(
      "/api/env/env%201/containers/web-1/logs/download?nodeId=node-a&since=2026-10-09T10%3A00%3A00.000Z&until=2026-10-09T11%3A00%3A00.000Z&timestamps=1",
    );
  });
});

describe("defaultRange", () => {
  const now = new Date("2026-10-09T12:00:00.000Z");
  it("is the viewer's range", () => {
    expect(defaultRange({ kind: "lines" }, now)).toEqual({ from: "", to: "" });
    expect(defaultRange({ kind: "all" }, now)).toEqual({ from: "", to: "" });
    expect(defaultRange({ kind: "last", preset: "1h" }, now)).toEqual({
      from: toLocalInput("2026-10-09T11:00:00.000Z"),
      to: "",
    });
    expect(
      defaultRange(
        { kind: "custom", from: "2026-10-09T08:00:00.000Z", to: "2026-10-09T09:00:00.000Z" },
        now,
      ),
    ).toEqual({
      from: toLocalInput("2026-10-09T08:00:00.000Z"),
      to: toLocalInput("2026-10-09T09:00:00.000Z"),
    });
  });
});

describe("DownloadPopover", () => {
  it("opens titled Download logs with From, To, Include timestamps and the UTC note", () => {
    render(<DownloadPopover target={target} show={{ kind: "lines" }} timestamps={false} />);
    open();
    expect(screen.getByRole("form", { name: "Download logs" })).toBeTruthy();
    expect(fromBox().value).toBe("");
    expect(toBox().value).toBe("");
    expect(screen.getByRole("checkbox", { name: "Include timestamps" })).toBeTruthy();
    expect(
      screen.getByText(
        "Downloads come straight from Docker, so they aren't limited to what's loaded here. Times are in UTC.",
      ),
    ).toBeTruthy();
    expect(screen.getByRole("button", { name: "Cancel" })).toBeTruthy();
    expect(submit()).toBeTruthy();
  });

  it("opens with the viewer's range and its Timestamps choice", () => {
    render(
      <DownloadPopover
        target={target}
        show={{ kind: "custom", from: "2026-10-09T08:00:00.000Z", to: "2026-10-09T09:00:00.000Z" }}
        timestamps={true}
      />,
    );
    open();
    expect(fromBox().value).toBe(toLocalInput("2026-10-09T08:00:00.000Z"));
    expect(toBox().value).toBe(toLocalInput("2026-10-09T09:00:00.000Z"));
    expect(
      (screen.getByRole("checkbox", { name: "Include timestamps" }) as HTMLElement).getAttribute(
        "aria-checked",
      ),
    ).toBe("true");
  });

  it("starts a browser download of the chosen range through an anchor", () => {
    render(<DownloadPopover target={target} show={{ kind: "lines" }} timestamps={false} />);
    open();
    fireEvent.change(fromBox(), { target: { value: "2026-10-09T08:00" } });
    fireEvent.change(toBox(), { target: { value: "2026-10-09T09:30" } });
    fireEvent.click(screen.getByRole("checkbox", { name: "Include timestamps" }));
    fireEvent.click(submit());

    expect(clicked).toHaveLength(1);
    const a = clicked[0] as HTMLAnchorElement;
    const url = new URL(a.href, "http://x");
    expect(url.pathname).toBe("/api/env/env%201/containers/web-1/logs/download");
    expect(url.searchParams.get("nodeId")).toBe("node-a");
    expect(url.searchParams.get("since")).toBe(new Date("2026-10-09T08:00").toISOString());
    expect(url.searchParams.get("until")).toBe(new Date("2026-10-09T09:30").toISOString());
    expect(url.searchParams.get("timestamps")).toBe("1");
    expect(a.hasAttribute("download")).toBe(true);
    // The anchor is only a means to start it: it does not stay on the page.
    expect(document.querySelector('a[href*="logs/download"]')).toBeNull();
  });

  it("shows the spinner for one second and then the button returns", () => {
    vi.useFakeTimers();
    render(<DownloadPopover target={target} show={{ kind: "lines" }} timestamps={false} />);
    open();
    fireEvent.click(submit());
    expect(submit().getAttribute("aria-busy")).toBe("true");
    expect(screen.getByRole("status", { name: "Starting the download" })).toBeTruthy();
    // A second press while it spins does not start a second download.
    fireEvent.click(submit());
    expect(clicked).toHaveLength(1);
    act(() => {
      vi.advanceTimersByTime(DOWNLOAD_SPINNER_MS);
    });
    expect(submit().getAttribute("aria-busy")).toBe("false");
    expect(screen.queryByRole("status", { name: "Starting the download" })).toBeNull();
  });

  it("refuses an end that is not after the start", () => {
    render(<DownloadPopover target={target} show={{ kind: "lines" }} timestamps={false} />);
    open();
    fireEvent.change(fromBox(), { target: { value: "2026-10-09T10:00" } });
    fireEvent.change(toBox(), { target: { value: "2026-10-09T09:00" } });
    expect(screen.getByRole("alert").textContent).toBe("The end must be after the start.");
    expect(submit().disabled).toBe(true);
    fireEvent.click(submit());
    expect(clicked).toHaveLength(0);
  });

  it("closes on Cancel without downloading", () => {
    render(<DownloadPopover target={target} show={{ kind: "lines" }} timestamps={false} />);
    open();
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("form", { name: "Download logs" })).toBeNull();
    expect(clicked).toHaveLength(0);
  });

  it("tells the person when the browser would not start the download", async () => {
    vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => {
      throw new Error("blocked");
    });
    const { toast } = await import("sonner");
    const err = vi.spyOn(toast, "error").mockImplementation(() => "id");
    render(<DownloadPopover target={target} show={{ kind: "lines" }} timestamps={false} />);
    open();
    fireEvent.click(submit());
    expect(err).toHaveBeenCalledTimes(1);
    expect(submit().getAttribute("aria-busy")).toBe("false");
  });
});

describe("DownloadDialog", () => {
  it("is the same form in a dialog, opened by the More menu", () => {
    const onOpenChange = vi.fn();
    render(
      <DownloadDialog
        open
        onOpenChange={onOpenChange}
        target={target}
        show={{ kind: "all" }}
        timestamps={false}
      />,
    );
    expect(screen.getByRole("dialog", { name: "Download logs" })).toBeTruthy();
    fireEvent.click(submit());
    expect(clicked).toHaveLength(1);
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(onOpenChange).toHaveBeenCalledWith(false);
  });
});
