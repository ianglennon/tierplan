// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import type { StackDiffHunk, StackDiffLine } from "@/api/stacks";
import { YamlDiff } from "@/features/stack-review/yaml-diff";
import "@/i18n";

// The rendered-YAML diff (plan 07-16, STK-06, D-11, A11): the hunks the server computed, drawn as
// a native table. The page imports no diff library; it only lays the server's hunks out.

afterEach(() => cleanup());

const ctx = (n: number, text: string): StackDiffLine => ({ kind: "context", old: n, new: n, text });

// A 12-line document whose line 6 changed from "replicas: 1" to "replicas: 2".
const DOC = [
  "services:",
  "  web:",
  "    image: nginx:1.27",
  "    deploy:",
  "      mode: replicated",
  "      replicas: 2",
  "    ports:",
  '      - "9090:80"',
  "    environment:",
  "      MODE: prod",
  "networks:",
  "  default: {}",
];

const oneHunk: StackDiffHunk = {
  oldStart: 3,
  oldLines: 7,
  newStart: 3,
  newLines: 7,
  lines: [
    ctx(3, DOC[2] as string),
    ctx(4, DOC[3] as string),
    ctx(5, DOC[4] as string),
    { kind: "removed", old: 6, new: 0, text: "      replicas: 1" },
    { kind: "added", old: 0, new: 6, text: "      replicas: 2" },
    ctx(7, DOC[6] as string),
    ctx(8, DOC[7] as string),
    ctx(9, DOC[8] as string),
  ],
};

const mount = (hunks: StackDiffHunk[] | null | undefined, lines: string[] | null = DOC) =>
  render(<YamlDiff hunks={hunks} lines={lines} caption="Rendered YAML, changes" />);

describe("the table", () => {
  it("is a native table captioned Rendered YAML, changes", () => {
    mount([oneHunk]);
    const table = screen.getByRole("table", { name: "Rendered YAML, changes" });
    expect(table.tagName).toBe("TABLE");
    expect(table.className).toContain("font-mono");
  });

  it("gives every line a row named for what it is and its number", () => {
    mount([oneHunk]);
    expect(screen.getByRole("row", { name: "Removed line 6" })).toBeTruthy();
    expect(screen.getByRole("row", { name: "Added line 6" })).toBeTruthy();
    expect(screen.getByRole("row", { name: "Unchanged line 3" })).toBeTruthy();
    expect(screen.getByRole("row", { name: "Unchanged line 9" })).toBeTruthy();
  });

  it("writes the change as text in the glyph column, so colour is not what says it", () => {
    mount([oneHunk]);
    const added = within(screen.getByRole("row", { name: "Added line 6" })).getAllByRole("cell");
    const removed = within(screen.getByRole("row", { name: "Removed line 6" })).getAllByRole(
      "cell",
    );
    expect(added.map((c) => c.textContent)).toEqual(["", "6", "+", "      replicas: 2"]);
    expect(removed.map((c) => c.textContent)).toEqual(["6", "", "−", "      replicas: 1"]);
    const same = within(screen.getByRole("row", { name: "Unchanged line 4" })).getAllByRole("cell");
    expect(same[2]?.textContent).toBe("");
  });

  it("sizes its lines at 18px with 48px gutters and a 24px glyph column", () => {
    mount([oneHunk]);
    const row = screen.getByRole("row", { name: "Added line 6" });
    expect(row.className).toContain("h-4.5");
    const cells = within(row).getAllByRole("cell");
    expect(cells[0]?.className).toContain("w-12");
    expect(cells[1]?.className).toContain("w-12");
    expect(cells[2]?.className).toContain("w-6");
    expect(cells[3]?.className).toContain("whitespace-pre-wrap");
  });

  it("draws markup in a line as text", () => {
    mount([
      {
        oldStart: 1,
        oldLines: 0,
        newStart: 1,
        newLines: 1,
        lines: [{ kind: "added", old: 0, new: 1, text: "<img src=x onerror=alert(1)>" }],
      },
    ]);
    expect(screen.getByText("<img src=x onerror=alert(1)>")).toBeTruthy();
    expect(document.querySelector("img")).toBeNull();
  });
});

describe("the unchanged lines", () => {
  it("offer to show the ones before and after the hunk, and name how many", () => {
    mount([oneHunk]);
    expect(screen.getByRole("button", { name: "Show 2 unchanged lines" })).toBeTruthy();
    expect(screen.getAllByRole("button")).toHaveLength(2);
    expect(screen.queryByRole("row", { name: "Unchanged line 1" })).toBeNull();
    expect(screen.queryByRole("row", { name: "Unchanged line 12" })).toBeNull();
  });

  it("show the lines of the newer text, numbered, in the place of the row", () => {
    mount([oneHunk]);
    const before = screen.getAllByRole("button", {
      name: "Show 2 unchanged lines",
    })[0] as HTMLElement;
    fireEvent.click(before);
    const first = screen.getByRole("row", { name: "Unchanged line 1" });
    expect(
      within(first)
        .getAllByRole("cell")
        .map((c) => c.textContent),
    ).toEqual(["1", "1", "", "services:"]);
    expect(screen.getByRole("row", { name: "Unchanged line 2" })).toBeTruthy();
    // The other row is still folded.
    expect(screen.getAllByRole("button")).toHaveLength(1);
    expect(screen.queryByRole("row", { name: "Unchanged line 11" })).toBeNull();
  });

  it("are counted between two hunks and numbered on both sides", () => {
    // The old text: a, b, b2, c..i, j. The new: a, B, c..i, X, j. Two lines went and one came, so
    // a line between the hunks has an old number one higher than its new number.
    const far: StackDiffHunk[] = [
      {
        oldStart: 1,
        oldLines: 3,
        newStart: 1,
        newLines: 2,
        lines: [
          ctx(1, "a"),
          { kind: "removed", old: 2, new: 0, text: "b" },
          { kind: "removed", old: 3, new: 0, text: "b2" },
          { kind: "added", old: 0, new: 2, text: "B" },
        ],
      },
      {
        oldStart: 11,
        oldLines: 1,
        newStart: 10,
        newLines: 2,
        lines: [{ kind: "added", old: 0, new: 10, text: "X" }, ctx(11, "j")],
      },
    ];
    const lines = ["a", "B", "c", "d", "e", "f", "g", "h", "i", "X", "j"];
    mount(far, lines);
    fireEvent.click(screen.getByRole("button", { name: "Show 7 unchanged lines" }));
    const row = screen.getByRole("row", { name: "Unchanged line 4" });
    expect(
      within(row)
        .getAllByRole("cell")
        .map((c) => c.textContent),
    ).toEqual(["5", "4", "", "d"]);
    expect(screen.getByRole("row", { name: "Unchanged line 3" })).toBeTruthy();
    expect(screen.getByRole("row", { name: "Unchanged line 9" })).toBeTruthy();
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("say how many there are, without a button, when the page was not given the text", () => {
    mount([oneHunk], null);
    expect(screen.queryByRole("button")).toBeNull();
    // Only what lies before the first hunk is known: where the text ends needs the text.
    expect(screen.getAllByText(/^Show \d+ unchanged lines$/)).toHaveLength(1);
  });
});

describe("nothing to show", () => {
  it("says the rendering is the same as at the last deploy", () => {
    mount([]);
    expect(screen.getByText("The rendered YAML is the same as at the last deploy.")).toBeTruthy();
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("treats a missing list as no hunks", () => {
    mount(null);
    expect(screen.getByText("The rendered YAML is the same as at the last deploy.")).toBeTruthy();
  });
});
