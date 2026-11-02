// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { StackProblem } from "@/api/stacks";
import { ProblemsPanel, problemsAnnounceMs } from "@/features/stack-editor/problems-panel";
import "@/i18n";

// The Problems panel (plan 07-06, STK-01, UI-SPEC "Editor" and "Lint announcements"): counts that
// agree in number, rows a screen reader can name, a click that opens the problem, and a polite live
// region that announces the counts once per change.

const problem = (over: Partial<StackProblem> = {}): StackProblem => ({
  severity: "error",
  code: "yaml_invalid",
  message: "Line 3: bad",
  file: "compose.yaml",
  line: 3,
  col: 1,
  endLine: 3,
  endCol: 2,
  ...over,
});

beforeEach(() => {
  vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

function panel(over: Partial<React.ComponentProps<typeof ProblemsPanel>> = {}) {
  const onOpen = vi.fn();
  const props = { problems: [], checking: false, failed: false, onOpen, ...over };
  const view = render(<ProblemsPanel {...props} />);
  return {
    onOpen,
    view,
    rerender: (next: typeof over) => view.rerender(<ProblemsPanel {...props} {...next} />),
  };
}

describe("the counts", () => {
  it("agree in number with the problems", () => {
    for (const [problems, want] of [
      [[problem()], "1 error, 0 warnings"],
      [
        [problem(), problem({ line: 4 }), problem({ severity: "warning", line: 5 })],
        "2 errors, 1 warning",
      ],
      [[problem({ severity: "warning" })], "0 errors, 1 warning"],
    ] as const) {
      const { view } = panel({ problems: [...problems] });
      expect(screen.getByText(want)).toBeTruthy();
      view.unmount();
    }
  });

  it("read No problems with a check mark at zero", () => {
    panel();
    expect(screen.getByText("No problems")).toBeTruthy();
    expect(document.querySelector("svg.lucide-circle-check")).not.toBeNull();
  });

  it("show nothing until the server has answered once", () => {
    panel({ checked: false, checking: true });
    expect(screen.queryByText("No problems")).toBeNull();
    expect(screen.getByText("Checking…")).toBeTruthy();
  });
});

describe("the rows", () => {
  it("are named by severity, message, file and line, and show file:line", () => {
    panel({
      problems: [
        problem(),
        problem({ severity: "warning", message: "Moving tag", file: ".env", line: 7 }),
      ],
    });
    const error = screen.getByRole("button", { name: "Error: Line 3: bad, compose.yaml line 3" });
    expect(error.textContent).toContain("compose.yaml:3");
    expect(error.textContent).toContain("Error");
    const warning = screen.getByRole("button", { name: "Warning: Moving tag, .env line 7" });
    expect(warning.textContent).toContain(".env:7");
  });

  it("open the problem when clicked", () => {
    const { onOpen } = panel({ problems: [problem()] });
    fireEvent.click(screen.getByRole("button", { name: /^Error: Line 3: bad/ }));
    expect(onOpen).toHaveBeenCalledWith(problem());
  });
});

describe("opening and closing", () => {
  const header = () => screen.getByRole("button", { name: /Problems/ });

  it("stays closed with no problems, opens itself on the first error and then keeps the person's choice", () => {
    const { rerender } = panel();
    expect(header().getAttribute("aria-expanded")).toBe("false");
    rerender({ problems: [problem()] });
    expect(header().getAttribute("aria-expanded")).toBe("true");
    fireEvent.click(header());
    expect(header().getAttribute("aria-expanded")).toBe("false");
    rerender({ problems: [problem(), problem({ line: 9 })] });
    expect(header().getAttribute("aria-expanded")).toBe("false");
  });

  it("does not open for a warning alone", () => {
    panel({ problems: [problem({ severity: "warning" })] });
    expect(header().getAttribute("aria-expanded")).toBe("false");
  });
});

describe("the status line", () => {
  it("shows a spinner and Checking… while a check is on its way, beside the last counts", () => {
    panel({ problems: [problem()], checking: true });
    expect(screen.getByText("Checking…")).toBeTruthy();
    expect(screen.getByText("1 error, 0 warnings")).toBeTruthy();
  });

  it("says the stack could not be checked and keeps the rows", () => {
    panel({ problems: [problem()], failed: true });
    expect(screen.getByText("Couldn't check the stack. It will retry.")).toBeTruthy();
    expect(screen.getByRole("button", { name: /^Error: Line 3: bad/ })).toBeTruthy();
  });
});

describe("the announcement", () => {
  const live = () => screen.getByRole("status");

  it("is a polite live region that announces the counts once, after they stop changing", () => {
    const { rerender } = panel();
    expect(live().getAttribute("aria-live")).toBe("polite");
    act(() => {
      vi.advanceTimersByTime(problemsAnnounceMs);
    });
    expect(live().textContent).toBe("No problems");

    rerender({ problems: [problem()] });
    act(() => {
      vi.advanceTimersByTime(problemsAnnounceMs - 1);
    });
    expect(live().textContent).toBe("No problems");
    act(() => {
      vi.advanceTimersByTime(1);
    });
    expect(live().textContent).toBe("1 error, 0 warnings");
  });

  it("says nothing until the server has answered", () => {
    panel({ checked: false });
    act(() => {
      vi.advanceTimersByTime(problemsAnnounceMs * 2);
    });
    expect(live().textContent).toBe("");
  });
});
