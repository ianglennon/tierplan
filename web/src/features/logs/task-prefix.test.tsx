// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { TooltipProvider } from "@/components/ui/tooltip";
import {
  formatTaskPrefix,
  MAX_PREFIX_CH,
  MIN_PREFIX_CH,
  PREFIX_COLOURS,
  prefixColour,
  prefixWidthCh,
  TaskPrefix,
} from "@/features/logs/task-prefix";
import "@/i18n";

// The task prefix of a service log line (plan 06-29, OBS-02, D-32, UI-SPEC "Task prefix
// palette").

afterEach(cleanup);

describe("prefixColour", () => {
  it("cycles the eight ANSI colours that are neither red nor green, in the documented order", () => {
    const want = [4, 5, 6, 3, 12, 13, 14, 11].map((n) => `var(--ansi-${n})`);
    expect(PREFIX_COLOURS).toEqual(want);
    for (let i = 0; i < 8; i++) expect(prefixColour(i)).toBe(want[i]);
  });

  it("wraps after eight and treats a negative index like the same place on the circle", () => {
    expect(prefixColour(8)).toBe(prefixColour(0));
    expect(prefixColour(19)).toBe(prefixColour(3));
    expect(prefixColour(-1)).toBe(prefixColour(7));
  });

  it("never uses ANSI red or green or their bright forms", () => {
    for (const c of PREFIX_COLOURS) expect(c).not.toMatch(/--ansi-(1|2|9|10)\)/);
  });
});

describe("formatTaskPrefix", () => {
  it("is slot@node beside the service header, and service.slot@node where the name is needed", () => {
    expect(formatTaskPrefix({ service: "web", slot: 2, node: "node3", showService: false })).toBe(
      "2@node3",
    );
    expect(formatTaskPrefix({ service: "web", slot: 2, node: "node3", showService: true })).toBe(
      "web.2@node3",
    );
  });

  it("is service@node for a task of a global service, whose slot is 0", () => {
    expect(formatTaskPrefix({ service: "agent", slot: 0, node: "node3", showService: false })).toBe(
      "agent@node3",
    );
  });

  it("falls back to the slot alone when the node is not known", () => {
    expect(formatTaskPrefix({ service: "web", slot: 2, node: "", showService: false })).toBe("2");
  });
});

describe("prefixWidthCh", () => {
  it("is the longest prefix, at least 12 and at most 28", () => {
    expect(MIN_PREFIX_CH).toBe(12);
    expect(MAX_PREFIX_CH).toBe(28);
    expect(prefixWidthCh(3)).toBe(12);
    expect(prefixWidthCh(20)).toBe(20);
    expect(prefixWidthCh(80)).toBe(28);
  });
});

function renderPrefix(text: string, props: Partial<Parameters<typeof TaskPrefix>[0]> = {}) {
  return render(
    <TooltipProvider>
      <TaskPrefix
        text={text}
        colour={prefixColour(0)}
        widthCh={12}
        taskId="abcdefghijklmnopqrstuvwxy"
        state="running"
        {...props}
      />
    </TooltipProvider>,
  );
}

describe("TaskPrefix", () => {
  it("is real text in its colour that cannot be selected", () => {
    renderPrefix("2@node3");
    const el = screen.getByText("2@node3");
    expect(el.className).toContain("select-none");
    expect(el.style.color).toBe("var(--ansi-4)");
    expect(el.style.width).toBe("12ch");
    expect(el.className).toContain("font-mono");
  });

  it("is plain text, never markup", () => {
    renderPrefix("<b>x</b>@n");
    expect(screen.getByText("<b>x</b>@n")).toBeTruthy();
    expect(document.querySelector("b")).toBeNull();
  });

  it("has no tooltip when it fits", () => {
    renderPrefix("2@node3");
    fireEvent.focus(screen.getByText("2@node3"));
    expect(screen.queryByRole("tooltip")).toBeNull();
  });

  it("truncates over 28 characters and shows the whole prefix, the task ID and the state in a tooltip", async () => {
    const long = "a-very-long-service-name.12@a-node-with-a-long-host-name";
    renderPrefix(long, { widthCh: 28 });
    const el = screen.getByText(long);
    expect(el.className).toContain("truncate");
    fireEvent.focus(el);
    const tip = await screen.findByRole("tooltip");
    expect(tip.textContent).toContain(long);
    expect(tip.textContent).toContain("abcdefghijklmnopqrstuvwxy");
    expect(tip.textContent).toContain("running");
  });
});
