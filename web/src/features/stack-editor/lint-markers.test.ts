// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { EditorState } from "@codemirror/state";
import { EditorView } from "@codemirror/view";
import { afterEach, describe, expect, it } from "vitest";

import {
  type LintProblem,
  lintMarkers,
  lintProblemsOf,
  setLintProblems,
  showLintTip,
} from "@/features/stack-editor/lint-markers";
import { varHighlight, variableRanges } from "@/features/stack-editor/var-highlight";
import "@/i18n";

// The markers a stack file's editor draws for what the check found (plan 07-08, STK-01, UI-SPEC
// "Editor line with an error / warning"): the same problems the Problems panel lists, drawn at their
// lines, with their message one hover or focus away and never as HTML.

const DOC = ["services:", "  web:", "    image: nginx:1.27", "    build: .", ""].join("\n");

const warning = (over: Partial<LintProblem> = {}): LintProblem => ({
  severity: "warning",
  message: "`build` is ignored on Swarm. Build and push the image, then set `image`.",
  line: 4,
  col: 5,
  endLine: 4,
  endCol: 10,
  ...over,
});

const error = (over: Partial<LintProblem> = {}): LintProblem => ({
  severity: "error",
  message: "web has build but no image. Build and push the image, then set image.",
  line: 3,
  col: 5,
  endLine: 3,
  endCol: 10,
  ...over,
});

const mounted: EditorView[] = [];

function editor(doc = DOC, problems: readonly LintProblem[] = []): EditorView {
  const view = new EditorView({
    state: EditorState.create({ doc, extensions: [lintMarkers(problems), varHighlight()] }),
    parent: document.body,
  });
  mounted.push(view);
  return view;
}

function show(view: EditorView, problems: readonly LintProblem[]) {
  view.dispatch({ effects: setLintProblems.of(problems) });
}

afterEach(() => {
  for (const v of mounted.splice(0)) v.destroy();
});

describe("lintMarkers", () => {
  it("draws a warning as a TriangleAlert marker and a tinted line, and no underline", () => {
    const view = editor();
    show(view, [warning()]);
    const marker = view.dom.querySelector(".cm-lint-marker-warning");
    expect(marker).not.toBeNull();
    expect(marker?.querySelector("svg")).not.toBeNull();
    expect(marker?.getAttribute("aria-label")).toBe(
      "Warning: `build` is ignored on Swarm. Build and push the image, then set `image`.",
    );
    expect(marker?.getAttribute("tabindex")).toBe("0");
    const tinted = view.dom.querySelectorAll(".cm-line.cm-lint-line-warning");
    expect(tinted).toHaveLength(1);
    expect(tinted[0]?.textContent).toBe("    build: .");
    expect(view.dom.querySelector(".cm-lint-range")).toBeNull();
  });

  it("draws an error as a CircleX marker, a tinted line and a wavy underline of its range", () => {
    const view = editor();
    show(view, [error()]);
    const marker = view.dom.querySelector(".cm-lint-marker-error");
    expect(marker?.getAttribute("aria-label")).toBe(
      "Error: web has build but no image. Build and push the image, then set image.",
    );
    expect(view.dom.querySelectorAll(".cm-line.cm-lint-line-error")).toHaveLength(1);
    expect(view.dom.querySelector(".cm-lint-range")?.textContent).toBe("image");
  });

  it("names every problem of a line on its one marker and lets the error win the icon", () => {
    const view = editor();
    show(view, [warning({ line: 3, message: "First" }), error({ line: 3, message: "Second" })]);
    expect(view.dom.querySelectorAll(".cm-lint-marker")).toHaveLength(1);
    const marker = view.dom.querySelector(".cm-lint-marker");
    expect(marker?.classList.contains("cm-lint-marker-error")).toBe(true);
    expect(marker?.getAttribute("aria-label")).toBe("Warning: First Error: Second");
    expect(view.dom.querySelectorAll(".cm-line.cm-lint-line-error")).toHaveLength(1);
    expect(view.dom.querySelector(".cm-line.cm-lint-line-warning")).toBeNull();
  });

  it("replaces the problems without recreating the editor or touching the text", () => {
    const view = editor();
    show(view, [warning()]);
    const dom = view.dom;
    show(view, [error()]);
    expect(view.dom).toBe(dom);
    expect(view.state.doc.toString()).toBe(DOC);
    expect(dom.querySelector(".cm-lint-marker-warning")).toBeNull();
    expect(dom.querySelector(".cm-lint-marker-error")).not.toBeNull();
    expect(lintProblemsOf(view)).toHaveLength(1);
    show(view, []);
    expect(dom.querySelector(".cm-lint-marker")).toBeNull();
    expect(dom.querySelector(".cm-lint-line-error")).toBeNull();
  });

  it("starts with the problems it is given", () => {
    const view = editor(DOC, [warning()]);
    expect(view.dom.querySelector(".cm-lint-marker-warning")).not.toBeNull();
  });

  it("shows the messages of a line in a tooltip when the marker is hovered or focused, and closes it", () => {
    const view = editor();
    show(view, [warning(), warning({ message: "Another" })]);
    view.dispatch({ effects: showLintTip.of(view.state.doc.line(4).from) });
    const tip = view.dom.querySelector(".cm-lint-tooltip");
    expect(tip?.getAttribute("role")).toBe("tooltip");
    expect(tip?.textContent).toContain("Warning: `build` is ignored on Swarm.");
    expect(tip?.textContent).toContain("Warning: Another");
    view.dispatch({ effects: showLintTip.of(null) });
    expect(view.dom.querySelector(".cm-lint-tooltip")).toBeNull();
  });

  it("opens no tooltip for a line without a problem", () => {
    const view = editor();
    show(view, [warning()]);
    view.dispatch({ effects: showLintTip.of(view.state.doc.line(1).from) });
    expect(view.dom.querySelector(".cm-lint-tooltip")).toBeNull();
  });

  it("sets a message as text, never as HTML", () => {
    const view = editor();
    show(view, [warning({ message: '<img src=x onerror="alert(1)"> <b>bold</b>' })]);
    view.dispatch({ effects: showLintTip.of(view.state.doc.line(4).from) });
    expect(view.dom.querySelector(".cm-lint-marker img, .cm-lint-tooltip img")).toBeNull();
    expect(view.dom.querySelector(".cm-lint-tooltip b")).toBeNull();
    expect(view.dom.querySelector(".cm-lint-tooltip")?.textContent).toContain("<b>bold</b>");
  });

  it("moves a marker with its line while the text is edited, and closes the tooltip", () => {
    const view = editor();
    show(view, [warning()]);
    view.dispatch({ effects: showLintTip.of(view.state.doc.line(4).from) });
    view.dispatch({ changes: { from: 0, insert: "# a comment\n" } });
    expect(view.dom.querySelector(".cm-lint-tooltip")).toBeNull();
    const tinted = view.dom.querySelector(".cm-line.cm-lint-line-warning");
    expect(tinted?.textContent).toBe("    build: .");
  });

  it("clamps a problem past the end of the file to the last line", () => {
    const view = editor("a: 1\nb: 2");
    show(view, [warning({ line: 99, endLine: 99 })]);
    expect(view.dom.querySelector(".cm-line.cm-lint-line-warning")?.textContent).toBe("b: 2");
  });

  it("still marks the line of an error whose range is empty", () => {
    const view = editor("a: 1\n");
    show(view, [error({ line: 2, col: 1, endLine: 2, endCol: 1 })]);
    expect(view.dom.querySelector(".cm-lint-marker-error")).not.toBeNull();
  });
});

describe("varHighlight", () => {
  it("finds the references and leaves an escaped one alone", () => {
    // Compose variable references, written in template literals with the brace escaped.
    const text = `ports: ['\${WEB_PORT:-8080}:80', '\${HOST}', '$\${LITERAL}', '$$$\${TWO}', '$\${A}\${B}']`;
    const found = variableRanges(text).map(([a, b]) => text.slice(a, b));
    expect(found).toEqual([`\${WEB_PORT:-8080}`, `\${HOST}`, `\${B}`]);
  });

  it("marks the references in the editor with the variable class", () => {
    const view = editor(`image: nginx:\${TAG:-1.27}\nports:\n  - '\${WEB_PORT:-8080}:80'\n`);
    const marked = [...view.dom.querySelectorAll(".cm-var-ref")].map((e) => e.textContent);
    expect(marked).toEqual([`\${TAG:-1.27}`, `\${WEB_PORT:-8080}`]);
  });
});
