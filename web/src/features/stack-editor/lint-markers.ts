// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import {
  type Extension,
  type Range,
  RangeSet,
  StateEffect,
  StateField,
  type Text,
} from "@codemirror/state";
import {
  Decoration,
  type DecorationSet,
  EditorView,
  GutterMarker,
  gutter,
  hoverTooltip,
  showTooltip,
  type Tooltip,
} from "@codemirror/view";

import i18n from "@/i18n";

// Inline problem markers for a stack file (plan 07-08, STK-01, UI-SPEC "Editor line with an error /
// warning"): a 16px gutter column with an icon, an 8% tint of the line and, for errors, a wavy
// underline of the range. The message is in a tooltip shown by hovering the line or the marker and
// by focusing the marker, and it is the same message the Problems panel lists, so nothing lives
// only on hover. Messages are set as text, never as HTML.
//
// This is built from @codemirror/view alone (gutter, Decoration, hoverTooltip, showTooltip); the
// UI-SPEC keeps @codemirror/lint out. The problems come in through a StateEffect, so a new check
// result never recreates the editor or loses its undo history.

// What a marker needs of a problem: its severity, text and range (1-based line and column).
export interface LintProblem {
  severity: string;
  message: string;
  line: number;
  col: number;
  endLine: number;
  endCol: number;
}

// setLintProblems replaces the problems drawn in an editor.
export const setLintProblems = StateEffect.define<readonly LintProblem[]>();

// showLintTip opens the tooltip of the line at a document position; null closes it.
export const showLintTip = StateEffect.define<number | null>();

const isError = (p: LintProblem) => p.severity === "error";

// The icons are Lucide's CircleX and TriangleAlert, drawn here as SVG because a CodeMirror
// gutter makes DOM, not React. They are decorative: the marker carries the label.
const SVG_NS = "http://www.w3.org/2000/svg";
const ICONS: Record<"error" | "warning", { tag: string; attrs: Record<string, string> }[]> = {
  error: [
    { tag: "circle", attrs: { cx: "12", cy: "12", r: "10" } },
    { tag: "path", attrs: { d: "m15 9-6 6" } },
    { tag: "path", attrs: { d: "m9 9 6 6" } },
  ],
  warning: [
    {
      tag: "path",
      attrs: { d: "m21.73 18-8-14a2 2 0 0 0-3.48 0l-8 14A2 2 0 0 0 4 21h16a2 2 0 0 0 1.73-3" },
    },
    { tag: "path", attrs: { d: "M12 9v4" } },
    { tag: "path", attrs: { d: "M12 17h.01" } },
  ],
};

function icon(kind: "error" | "warning"): SVGElement {
  const svg = document.createElementNS(SVG_NS, "svg");
  for (const [k, v] of Object.entries({
    viewBox: "0 0 24 24",
    width: "14",
    height: "14",
    fill: "none",
    stroke: "currentColor",
    "stroke-width": "2",
    "stroke-linecap": "round",
    "stroke-linejoin": "round",
    "aria-hidden": "true",
  })) {
    svg.setAttribute(k, v);
  }
  for (const part of ICONS[kind]) {
    const el = document.createElementNS(SVG_NS, part.tag);
    for (const [k, v] of Object.entries(part.attrs)) el.setAttribute(k, v);
    svg.append(el);
  }
  return svg;
}

const severityWord = (p: LintProblem) =>
  String(
    i18n.t(isError(p) ? "stackProblems.severity.error" : "stackProblems.severity.warning", {
      ns: "stackProblems",
    }),
  );

const markerName = (list: readonly LintProblem[]) =>
  list
    .map((p) =>
      String(
        i18n.t("stackProblems.marker", {
          ns: "stackProblems",
          severity: severityWord(p),
          message: p.message,
        }),
      ),
    )
    .join(" ");

// The gutter marker of one line: the icon of its worst problem, a focusable element named by
// every message of the line. Two markers are equal when they would draw the same thing.
class LintMarker extends GutterMarker {
  readonly error: boolean;
  constructor(readonly list: readonly LintProblem[]) {
    super();
    this.error = list.some(isError);
  }
  eq(other: GutterMarker): boolean {
    return (
      other instanceof LintMarker &&
      other.error === this.error &&
      markerName(other.list) === markerName(this.list)
    );
  }
  toDOM(): Node {
    const el = document.createElement("span");
    el.className = `cm-lint-marker cm-lint-marker-${this.error ? "error" : "warning"}`;
    el.tabIndex = 0;
    el.setAttribute("role", "img");
    el.setAttribute("aria-label", markerName(this.list));
    el.append(icon(this.error ? "error" : "warning"));
    return el;
  }
}

interface LintState {
  problems: readonly LintProblem[];
  decorations: DecorationSet;
  markers: RangeSet<GutterMarker>;
  // The document position of the line whose tooltip is open, or null.
  tip: number | null;
}

// clampLine is the line of the document a 1-based line number names, a line past the end being the last.
const clampLine = (doc: Text, n: number) => doc.line(Math.min(Math.max(n, 1), doc.lines));

function build(
  doc: Text,
  problems: readonly LintProblem[],
): Pick<LintState, "decorations" | "markers"> {
  const byLine = new Map<number, LintProblem[]>();
  const decos: Range<Decoration>[] = [];
  for (const p of problems) {
    const line = clampLine(doc, p.line);
    const list = byLine.get(line.from);
    if (list) list.push(p);
    else byLine.set(line.from, [p]);
    if (isError(p)) {
      // The wavy underline is for errors only; a range that is empty or leaves its line marks the
      // first character.
      const end = clampLine(doc, p.endLine);
      const from = Math.min(line.from + Math.max(p.col - 1, 0), line.to);
      let to = end.from + Math.max(p.endCol - 1, 0);
      to = Math.min(Math.max(to, from + 1), end.to);
      if (to > from) decos.push(Decoration.mark({ class: "cm-lint-range" }).range(from, to));
    }
  }
  const markers: Range<GutterMarker>[] = [];
  for (const [from, list] of byLine) {
    const cls = list.some(isError) ? "cm-lint-line-error" : "cm-lint-line-warning";
    decos.push(Decoration.line({ class: cls }).range(from));
    markers.push(new LintMarker(list).range(from));
  }
  return {
    decorations: Decoration.set(decos, true),
    markers: RangeSet.of(markers, true),
  };
}

// problemsOnLine are the problems whose line is the one that holds pos.
function problemsOnLine(state: { doc: Text }, problems: readonly LintProblem[], pos: number) {
  const line = state.doc.lineAt(pos);
  return problems.filter((p) => clampLine(state.doc, p.line).from === line.from);
}

// tooltipDom is the content of a tooltip: a row for each problem, severity word then message.
function tooltipDom(list: readonly LintProblem[]): { dom: HTMLElement } {
  const dom = document.createElement("div");
  dom.className = "cm-lint-tooltip";
  dom.setAttribute("role", "tooltip");
  for (const p of list) {
    const row = document.createElement("div");
    row.className = `cm-lint-tooltip-row cm-lint-tooltip-${isError(p) ? "error" : "warning"}`;
    const word = document.createElement("strong");
    word.textContent = `${severityWord(p)}: `;
    const text = document.createElement("span");
    text.textContent = p.message;
    row.append(word, text);
    dom.append(row);
  }
  return { dom };
}

const lintState = StateField.define<LintState>({
  create: (state) => ({
    problems: [],
    ...build(state.doc, []),
    tip: null,
  }),
  update(value, tr) {
    let next = value;
    for (const e of tr.effects) {
      if (e.is(setLintProblems)) {
        next = { problems: e.value, ...build(tr.state.doc, e.value), tip: null };
      } else if (e.is(showLintTip)) {
        next = { ...next, tip: e.value };
      }
    }
    if (tr.docChanged && next === value) {
      // Typing moves the lines under the markers; they follow until the next check replaces
      // them, and an open tooltip closes rather than describing a line that moved.
      next = {
        ...value,
        decorations: value.decorations.map(tr.changes),
        markers: value.markers.map(tr.changes),
        tip: null,
      };
    }
    return next;
  },
  provide: (f) => [
    EditorView.decorations.from(f, (v) => v.decorations),
    showTooltip.compute([f], (state): Tooltip | null => {
      const v = state.field(f);
      if (v.tip === null || v.tip > state.doc.length) return null;
      const list = problemsOnLine(state, v.problems, v.tip);
      if (list.length === 0) return null;
      return { pos: v.tip, above: true, strictSide: false, create: () => tooltipDom(list) };
    }),
  ],
});

const lintGutter = gutter({
  class: "cm-lint-gutter",
  markers: (view) => view.state.field(lintState).markers,
  // Hovering or focusing a marker opens the tooltip of its line; leaving or blurring closes it.
  domEventHandlers: {
    mouseover: (view, line) => {
      view.dispatch({ effects: showLintTip.of(line.from) });
      return false;
    },
    focusin: (view, line) => {
      view.dispatch({ effects: showLintTip.of(line.from) });
      return false;
    },
    mouseleave: (view) => {
      view.dispatch({ effects: showLintTip.of(null) });
      return false;
    },
    focusout: (view) => {
      view.dispatch({ effects: showLintTip.of(null) });
      return false;
    },
  },
});

// Hovering the text of a line with a problem shows its messages.
const hoverLint = hoverTooltip((view, pos) => {
  const list = problemsOnLine(view.state, view.state.field(lintState).problems, pos);
  if (list.length === 0) return null;
  return { pos, above: true, create: () => tooltipDom(list) };
});

// The colours are the status variables the page already has: the tints are 8% of the offline or
// warning colour over the card, the underline is the offline text colour (UI-SPEC "Color").
const lintTheme = EditorView.baseTheme({
  ".cm-lint-gutter": { width: "16px" },
  ".cm-lint-gutter .cm-gutterElement": {
    display: "flex",
    alignItems: "center",
    justifyContent: "center",
  },
  ".cm-lint-marker": { display: "inline-flex", borderRadius: "var(--radius-sm)", outline: "none" },
  ".cm-lint-marker:focus-visible": { outline: "2px solid var(--ring)" },
  ".cm-lint-marker-error": { color: "var(--status-offline-text)" },
  ".cm-lint-marker-warning": { color: "var(--status-warning-text)" },
  ".cm-line.cm-lint-line-error": {
    backgroundColor: "color-mix(in srgb, var(--status-offline) 8%, var(--card))",
  },
  ".cm-line.cm-lint-line-warning": {
    backgroundColor: "color-mix(in srgb, var(--status-warning) 8%, var(--card))",
  },
  ".cm-lint-range": {
    textDecoration: "underline wavy var(--status-offline-text)",
    textUnderlineOffset: "3px",
  },
  ".cm-tooltip:has(> .cm-lint-tooltip)": {
    backgroundColor: "var(--popover)",
    color: "var(--popover-foreground)",
    border: "1px solid var(--border)",
    borderRadius: "var(--radius-md)",
    fontFamily: "var(--font-sans)",
    fontSize: "12px",
    padding: "6px 8px",
    maxWidth: "min(480px, 90vw)",
    overflowWrap: "anywhere",
  },
  ".cm-lint-tooltip-error strong": { color: "var(--status-offline-text)" },
  ".cm-lint-tooltip-warning strong": { color: "var(--status-warning-text)" },
});

// lintMarkers is the extension that draws problems; give it the problems of the open file at the
// start and push later ones with setLintProblems.
export function lintMarkers(problems: readonly LintProblem[] = []): Extension {
  return [
    lintState.init((state) => ({ problems, ...build(state.doc, problems), tip: null })),
    lintGutter,
    hoverLint,
    lintTheme,
  ];
}

// lintProblemsOf reads back the problems an editor is drawing, for tests and for callers that
// need to know whether an editor already shows a result.
export function lintProblemsOf(view: EditorView): readonly LintProblem[] {
  return view.state.field(lintState, false)?.problems ?? [];
}
