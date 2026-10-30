// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { LogToolbar, type LogToolbarProps } from "@/features/logs/log-toolbar";
import "@/i18n";

// The log viewer's toolbar (plan 04-20, D-15, D-17, D-19, UI-SPEC "Log viewer").

const realWidth = window.innerWidth;

afterEach(() => {
  cleanup();
  window.innerWidth = realWidth;
});

function props(over: Partial<LogToolbarProps> = {}): LogToolbarProps {
  return {
    show: { kind: "lines" },
    onShowChange: vi.fn(),
    query: "",
    onQueryChange: vi.fn(),
    regex: false,
    onRegexChange: vi.fn(),
    caseSensitive: false,
    onCaseSensitiveChange: vi.fn(),
    onlyMatching: false,
    onOnlyMatchingChange: vi.fn(),
    invalidPattern: false,
    matchCount: 0,
    currentMatch: 0,
    onNextMatch: vi.fn(),
    onPreviousMatch: vi.fn(),
    announcement: "",
    wrap: false,
    onWrapChange: vi.fn(),
    timestamps: false,
    onTimestampsChange: vi.fn(),
    utc: false,
    onUtcChange: vi.fn(),
    ...over,
  };
}

function renderBar(over: Partial<LogToolbarProps> = {}) {
  const p = props(over);
  render(<LogToolbar {...p} />);
  return p;
}

const button = (name: string | RegExp) => screen.getByRole("button", { name }) as HTMLButtonElement;

describe("Show", () => {
  it("offers the six ranges, with the last 500 lines first and chosen", () => {
    renderBar();
    const select = screen.getByRole("combobox", { name: "Show" });
    expect(select.textContent).toContain("Last 500 lines");
    fireEvent.keyDown(select, { key: "Enter" });
    const options = screen.getAllByRole("option").map((o) => o.textContent);
    expect(options).toEqual([
      "Last 500 lines",
      "Last 5 minutes",
      "Last hour",
      "Last 24 hours",
      "All",
      "Custom…",
    ]);
  });

  it("changes the range to the one chosen", () => {
    const p = renderBar();
    fireEvent.keyDown(screen.getByRole("combobox", { name: "Show" }), { key: "Enter" });
    fireEvent.click(screen.getByRole("option", { name: "Last hour" }));
    expect(p.onShowChange).toHaveBeenCalledWith({ kind: "last", preset: "1h" });
  });

  it("names a custom range that is in force", () => {
    renderBar({ show: { kind: "custom", from: "2026-10-09T10:00:00.000Z" } });
    expect(screen.getByRole("combobox", { name: "Show" }).textContent).toContain("Custom…");
  });

  describe("Custom…", () => {
    function openCustom() {
      const p = renderBar();
      fireEvent.keyDown(screen.getByRole("combobox", { name: "Show" }), { key: "Enter" });
      fireEvent.click(screen.getByRole("option", { name: "Custom…" }));
      return p;
    }

    it("asks for From and an optional To, and applies only a valid range", () => {
      const p = openCustom();
      const from = screen.getByLabelText("From") as HTMLInputElement;
      const to = screen.getByLabelText("To (optional)") as HTMLInputElement;
      expect(from.type).toBe("datetime-local");
      expect(to.type).toBe("datetime-local");
      expect(button("Apply range").disabled).toBe(true);

      fireEvent.change(from, { target: { value: "2026-10-09T10:00" } });
      expect(button("Apply range").disabled).toBe(false);
      fireEvent.click(button("Apply range"));
      expect(p.onShowChange).toHaveBeenCalledTimes(1);
      const arg = (p.onShowChange as ReturnType<typeof vi.fn>).mock.calls[0]?.[0];
      expect(arg.kind).toBe("custom");
      expect(Date.parse(arg.from)).toBe(new Date("2026-10-09T10:00").getTime());
      expect(arg.to).toBeUndefined();
    });

    it("says the end must be after the start and does not apply", () => {
      const p = openCustom();
      fireEvent.change(screen.getByLabelText("From"), { target: { value: "2026-10-09T10:00" } });
      fireEvent.change(screen.getByLabelText("To (optional)"), {
        target: { value: "2026-10-09T09:00" },
      });
      expect(screen.getByText("The end must be after the start.")).toBeTruthy();
      expect(button("Apply range").disabled).toBe(true);
      fireEvent.change(screen.getByLabelText("To (optional)"), {
        target: { value: "2026-10-09T11:00" },
      });
      expect(screen.queryByText("The end must be after the start.")).toBeNull();
      fireEvent.click(button("Apply range"));
      const arg = (p.onShowChange as ReturnType<typeof vi.fn>).mock.calls[0]?.[0];
      expect(Date.parse(arg.to)).toBe(new Date("2026-10-09T11:00").getTime());
    });

    it("leaves the range as it was when cancelled", () => {
      const p = openCustom();
      fireEvent.click(button("Cancel"));
      expect(p.onShowChange).not.toHaveBeenCalled();
      expect(screen.queryByLabelText("From")).toBeNull();
    });
  });
});

describe("the view toggles", () => {
  it("are Wrap, Timestamps and UTC, each pressed or not", () => {
    renderBar({ wrap: true, timestamps: true });
    expect(button("Wrap").getAttribute("aria-pressed")).toBe("true");
    expect(button("Timestamps").getAttribute("aria-pressed")).toBe("true");
    expect(button("UTC").getAttribute("aria-pressed")).toBe("false");
  });

  it("report a press", () => {
    const p = renderBar({ timestamps: true });
    fireEvent.click(button("Wrap"));
    expect(p.onWrapChange).toHaveBeenCalledWith(true);
    fireEvent.click(button("UTC"));
    expect(p.onUtcChange).toHaveBeenCalledWith(true);
  });

  it("leave UTC off and unreachable while timestamps are off", () => {
    renderBar({ timestamps: false, utc: true });
    expect(button("UTC").disabled).toBe(true);
    expect(button("UTC").getAttribute("aria-pressed")).toBe("false");
  });

  it("are one toolbar the arrow keys move around", () => {
    renderBar({ timestamps: true });
    expect(screen.getByRole("toolbar")).toBeTruthy();
    button("Wrap").focus();
    fireEvent.keyDown(button("Wrap"), { key: "ArrowRight" });
    expect(document.activeElement).toBe(button("Timestamps"));
    fireEvent.keyDown(button("Timestamps"), { key: "ArrowLeft" });
    expect(document.activeElement).toBe(button("Wrap"));
    fireEvent.keyDown(button("Wrap"), { key: "End" });
    expect(document.activeElement).toBe(button("UTC"));
    fireEvent.keyDown(button("UTC"), { key: "Home" });
    expect(document.activeElement).toBe(button("Match case"));
  });

  it("show the live chip when the stream is live", () => {
    renderBar({ live: "live" });
    expect(screen.getByText("Live")).toBeTruthy();
  });

  it("give the second row's right-hand end to the actions it is given", () => {
    renderBar({ actions: <button type="button">Download…</button> });
    expect(button("Download…")).toBeTruthy();
  });
});

describe("search", () => {
  it("has a labelled box with placeholder, Match case and Use regular expression inside it", () => {
    const p = renderBar({ caseSensitive: true });
    const box = screen.getByRole("searchbox", { name: "Search logs" }) as HTMLInputElement;
    expect(box.placeholder).toBe("Search loaded lines");
    expect(button("Match case").getAttribute("aria-pressed")).toBe("true");
    expect(button("Use regular expression").getAttribute("aria-pressed")).toBe("false");
    fireEvent.click(button("Use regular expression"));
    expect(p.onRegexChange).toHaveBeenCalledWith(true);
    fireEvent.change(box, { target: { value: "err" } });
    expect(p.onQueryChange).toHaveBeenCalledWith("err");
  });

  it("counts: nothing before a query, No matches, 1 of 1, then current of total", () => {
    const { rerender } = render(<LogToolbar {...props()} />);
    expect(screen.queryByText("No matches")).toBeNull();
    rerender(<LogToolbar {...props({ query: "x", matchCount: 0 })} />);
    expect(screen.getByText("No matches")).toBeTruthy();
    rerender(<LogToolbar {...props({ query: "x", matchCount: 1, currentMatch: 0 })} />);
    expect(screen.getByText("1 of 1")).toBeTruthy();
    rerender(<LogToolbar {...props({ query: "x", matchCount: 5, currentMatch: 1 })} />);
    expect(screen.getByText("2 of 5")).toBeTruthy();
  });

  it("disables Previous and Next under two matches", () => {
    const { rerender } = render(<LogToolbar {...props({ query: "x", matchCount: 1 })} />);
    expect(button("Previous match").disabled).toBe(true);
    expect(button("Next match").disabled).toBe(true);
    rerender(<LogToolbar {...props({ query: "x", matchCount: 2 })} />);
    expect(button("Previous match").disabled).toBe(false);
    expect(button("Next match").disabled).toBe(false);
  });

  it("moves with the buttons, Enter and Shift+Enter", () => {
    const p = renderBar({ query: "x", matchCount: 3 });
    fireEvent.click(button("Next match"));
    expect(p.onNextMatch).toHaveBeenCalledTimes(1);
    fireEvent.click(button("Previous match"));
    expect(p.onPreviousMatch).toHaveBeenCalledTimes(1);
    const box = screen.getByRole("searchbox");
    fireEvent.keyDown(box, { key: "Enter" });
    expect(p.onNextMatch).toHaveBeenCalledTimes(2);
    fireEvent.keyDown(box, { key: "Enter", shiftKey: true });
    expect(p.onPreviousMatch).toHaveBeenCalledTimes(2);
  });

  it("says an invalid pattern is not valid, in place of the count", () => {
    renderBar({ query: "(", regex: true, invalidPattern: true });
    expect(screen.getByText("That isn't a valid pattern.")).toBeTruthy();
    expect(screen.queryByText("No matches")).toBeNull();
    expect(screen.getByRole("searchbox").getAttribute("aria-invalid")).toBe("true");
  });

  it("has the Only matching lines toggle", () => {
    const p = renderBar();
    expect(button("Only matching lines").getAttribute("aria-pressed")).toBe("false");
    fireEvent.click(button("Only matching lines"));
    expect(p.onOnlyMatchingChange).toHaveBeenCalledWith(true);
  });

  it("speaks the count politely, through a status region", () => {
    renderBar({ announcement: "2 of 5" });
    const polite = screen.getAllByRole("status").find((s) => s.textContent === "2 of 5");
    expect(polite?.getAttribute("aria-live")).toBe("polite");
  });
});

describe("under 768px", () => {
  function narrow() {
    window.innerWidth = 375;
  }

  it("keeps the live chip and moves Wrap, Timestamps and UTC into a View popover", () => {
    narrow();
    renderBar({ live: "live" });
    expect(screen.getByText("Live")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Wrap" })).toBeNull();
    fireEvent.click(button("View"));
    const popover = screen.getByRole("dialog");
    expect(within(popover).getByRole("button", { name: "Wrap" })).toBeTruthy();
    expect(within(popover).getByRole("button", { name: "Timestamps" })).toBeTruthy();
    expect(within(popover).getByRole("button", { name: "UTC" })).toBeTruthy();
  });

  it("has a More menu only when it has items to put in it, and moves the actions there", () => {
    narrow();
    const { unmount } = render(
      <LogToolbar {...props({ actions: <button type="button">Download…</button> })} />,
    );
    expect(screen.queryByRole("button", { name: "More" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Download…" })).toBeNull();
    unmount();
    cleanup();
    render(
      <LogToolbar
        {...props({
          moreItems: (
            <button type="button" role="menuitem">
              Download…
            </button>
          ),
        })}
      />,
    );
    expect(button("More")).toBeTruthy();
  });
});

describe("Open in new window", () => {
  it("is a button at the right of the second row, calling for the pop-out", () => {
    const onOpenPopout = vi.fn();
    renderBar({ onOpenPopout });
    fireEvent.click(button("Open in new window"));
    expect(onOpenPopout).toHaveBeenCalledTimes(1);
  });

  it("comes after the other actions", () => {
    renderBar({ onOpenPopout: vi.fn(), actions: <button type="button">Download…</button> });
    const names = within(screen.getByRole("toolbar"))
      .getAllByRole("button")
      .map((b) => b.textContent)
      .filter((n) => n === "Download…" || n === "Open in new window");
    expect(names).toEqual(["Download…", "Open in new window"]);
  });

  it("is not there without a pop-out to open", () => {
    renderBar();
    expect(screen.queryByRole("button", { name: "Open in new window" })).toBeNull();
  });

  it("is an item of the More menu under 768px, and the only one a pop-out leaves", () => {
    window.innerWidth = 500;
    const onOpenPopout = vi.fn();
    renderBar({ onOpenPopout });
    expect(screen.queryByRole("button", { name: "Open in new window" })).toBeNull();
    fireEvent.keyDown(button("More"), { key: "Enter" });
    fireEvent.click(screen.getByRole("menuitem", { name: "Open in new window" }));
    expect(onOpenPopout).toHaveBeenCalledTimes(1);
  });
});
