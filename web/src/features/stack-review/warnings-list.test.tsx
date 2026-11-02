// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import type { StackProblem } from "@/api/stacks";
import { WarningsList } from "@/features/stack-review/warnings-list";
import "@/i18n";
import { TestRouter } from "@/test-router";

// The warnings of a review (plan 07-15, D-10): a collapsible card that never blocks.

afterEach(() => cleanup());

const warning = (n: number, over: Partial<StackProblem> = {}): StackProblem => ({
  code: "w",
  col: 1,
  endCol: 2,
  endLine: n,
  file: "compose.yaml",
  line: n,
  message: `Warning ${n}.`,
  severity: "warning",
  ...over,
});

function mount(problems: StackProblem[]) {
  render(
    <TestRouter>
      <WarningsList problems={problems} envId="e1" stackId="s1" />
    </TestRouter>,
  );
}

const many = (n: number) => Array.from({ length: n }, (_, i) => warning(i + 1));

describe("WarningsList", () => {
  it("is titled with the number of warnings, plural-aware", () => {
    mount(many(2));
    expect(screen.getByText("2 warnings")).toBeTruthy();
    cleanup();
    mount(many(1));
    expect(screen.getByText("1 warning")).toBeTruthy();
  });

  it("is open by default for five warnings or fewer, with each message and a file:line link", () => {
    mount(many(5));
    expect(screen.getByRole("button", { name: /5 warnings/ }).getAttribute("aria-expanded")).toBe(
      "true",
    );
    expect(screen.getByText("Warning 3.")).toBeTruthy();
    expect(screen.getByRole("link", { name: "compose.yaml:3" }).getAttribute("href")).toBe(
      "/env/e1/stacks/s1/editor?file=compose.yaml&line=3",
    );
  });

  it("is closed for more than five, and opens on a press", () => {
    mount(many(6));
    const button = screen.getByRole("button", { name: /6 warnings/ });
    expect(button.getAttribute("aria-expanded")).toBe("false");
    expect(screen.queryByText("Warning 1.")).toBeNull();
    fireEvent.click(button);
    expect(screen.getByText("Warning 1.")).toBeTruthy();
  });

  it("lists warnings only, and is not an alert because it never blocks", () => {
    mount([warning(1), warning(2, { severity: "error", message: "An error." })]);
    expect(screen.getByText("1 warning")).toBeTruthy();
    expect(screen.queryByText("An error.")).toBeNull();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("is not drawn when there are no warnings", () => {
    mount([]);
    expect(screen.queryByRole("button")).toBeNull();
  });
});
