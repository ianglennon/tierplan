// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { EditorFooter } from "@/features/service-editor/editor-footer";

// G-09-10 (.planning/debug/stack-editor-footer-overlap-375.md): the footer was one row with a fixed
// height and no wrapping, so a status line and three buttons were squeezed until the text was
// drawn under a button. jsdom has no layout engine, so this pins the classes that make the bar
// wrap; the 375px, 320px and 768px geometry is checked in e2e/tests/responsive.spec.ts.

afterEach(() => cleanup());

function tokens(el: Element): Set<string> {
  return new Set(el.className.split(/\s+/).filter(Boolean));
}

describe("EditorFooter", () => {
  it("is a wrapping bar with a minimum height, not a fixed one (G-09-10)", () => {
    render(
      <EditorFooter>
        <p>Some status text</p>
        <button type="button">Go</button>
      </EditorFooter>,
    );
    const bar = document.querySelector('[data-slot="editor-footer"]');
    expect(bar).not.toBeNull();
    if (!bar) return;
    const classes = tokens(bar);
    for (const want of [
      "flex",
      "flex-wrap",
      "min-h-12",
      "max-md:min-h-14",
      "py-1",
      "sticky",
      "bottom-0",
      "items-center",
      "justify-end",
      "gap-2",
    ]) {
      expect(classes.has(want), `the bar has ${want}`).toBe(true);
    }
    for (const unwanted of ["h-12", "max-md:h-14", "flex-nowrap"]) {
      expect(classes.has(unwanted), `the bar has no ${unwanted}`).toBe(false);
    }
  });

  it("renders its children in order", () => {
    render(
      <EditorFooter>
        <p>First</p>
        <button type="button">Second</button>
      </EditorFooter>,
    );
    const bar = document.querySelector('[data-slot="editor-footer"]');
    expect(bar?.children[0]).toBe(screen.getByText("First"));
    expect(bar?.children[1]).toBe(screen.getByRole("button", { name: "Second" }));
  });
});
