// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import type { StackProblem } from "@/api/stacks";
import { BlockingList } from "@/features/stack-review/blocking-list";
import "@/i18n";
import { TestRouter } from "@/test-router";

// The errors that stop a deploy (plan 07-15, D-10): every one with its file and line, each a link
// that opens the editor at that line.

afterEach(() => cleanup());

const problem = (over: Partial<StackProblem>): StackProblem => ({
  code: "x",
  col: 1,
  endCol: 2,
  endLine: 1,
  file: "compose.yaml",
  line: 1,
  message: "Something is wrong.",
  severity: "error",
  ...over,
});

function mount(problems: StackProblem[]) {
  render(
    <TestRouter>
      <BlockingList problems={problems} envId="e1" stackId="s1" />
    </TestRouter>,
  );
}

describe("BlockingList", () => {
  it("is an alert titled with the number of errors, plural-aware", () => {
    mount([problem({ message: "A" }), problem({ message: "B", line: 3 })]);
    const alert = screen.getByRole("alert");
    expect(within(alert).getByText("2 errors stop this deploy")).toBeTruthy();
    cleanup();
    mount([problem({ message: "A" })]);
    expect(screen.getByText("1 error stops this deploy")).toBeTruthy();
  });

  it("lists each error with a file:line link that opens the editor at that line", () => {
    mount([problem({ message: "Image is missing.", file: "compose.yaml", line: 4 })]);
    const link = screen.getByRole("link", { name: "compose.yaml:4" });
    expect(link.getAttribute("href")).toBe("/env/e1/stacks/s1/editor?file=compose.yaml&line=4");
    expect(screen.getByText("Image is missing.")).toBeTruthy();
  });

  it("opens the file an error is in, not only compose.yaml", () => {
    mount([problem({ file: "config/app.env", line: 2 })]);
    expect(screen.getByRole("link", { name: "config/app.env:2" }).getAttribute("href")).toBe(
      "/env/e1/stacks/s1/editor?file=config%2Fapp.env&line=2",
    );
  });

  it("offers Back to editing", () => {
    mount([problem({})]);
    expect(screen.getByRole("link", { name: "Back to editing" }).getAttribute("href")).toBe(
      "/env/e1/stacks/s1/editor",
    );
  });

  it("lists errors only, not warnings", () => {
    mount([
      problem({ message: "Real." }),
      problem({ message: "Just a note.", severity: "warning" }),
    ]);
    expect(screen.getByText("1 error stops this deploy")).toBeTruthy();
    expect(screen.queryByText("Just a note.")).toBeNull();
  });

  it("is not drawn when there are no errors", () => {
    mount([problem({ severity: "warning" })]);
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("draws a message with markup in it as text, and an error with no line as text alone", () => {
    mount([problem({ message: "<b>bad</b>", line: 0, file: "" })]);
    expect(screen.getByText("<b>bad</b>").querySelector("b")).toBeNull();
    expect(screen.queryByRole("link", { name: /:/ })).toBeNull();
  });

  it("links a no-free-block problem to the address space", () => {
    const message =
      "network backend can't get a /26 subnet: no free block is left in 10.0.0.0/24. Free up space, or choose a smaller size in this environment's settings.";
    mount([problem({ code: "small_subnet_no_free_block", message, line: 1 })]);
    expect(screen.getByText(message)).toBeTruthy();
    expect(screen.getByRole("link", { name: "Open address space" }).getAttribute("href")).toBe(
      "/env/e1/address-space",
    );
    expect(screen.queryByRole("link", { name: "compose.yaml:1" })).toBeNull();
  });

  it("offers no address space link on any other error", () => {
    mount([problem({})]);
    expect(screen.queryByRole("link", { name: "Open address space" })).toBeNull();
  });
});
