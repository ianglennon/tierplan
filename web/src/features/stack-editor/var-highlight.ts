// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { Extension } from "@codemirror/state";
import {
  Decoration,
  type DecorationSet,
  EditorView,
  ViewPlugin,
  type ViewUpdate,
} from "@codemirror/view";

// Variable references in a stack file (plan 07-08, UI-SPEC "Code syntax colours"): ${NAME} and
// ${NAME:-default} are drawn in --ansi-5 at weight 600, so an interpolation stands out from a plain
// string. A reference whose dollar sign is escaped ($${NAME}) is a literal and is left alone.
const REFERENCE = /\$\{[^}\n]*\}/g;

const mark = Decoration.mark({ class: "cm-var-ref" });

// variableRanges are the [from, to) ranges of the references in text.
export function variableRanges(text: string): [number, number][] {
  const out: [number, number][] = [];
  for (const m of text.matchAll(REFERENCE)) {
    // An even run of dollar signs before the brace escapes nothing; an odd run escapes the last.
    let dollars = 0;
    while (m.index - dollars - 1 >= 0 && text[m.index - dollars - 1] === "$") dollars++;
    if (dollars % 2 === 1) continue;
    out.push([m.index, m.index + m[0].length]);
  }
  return out;
}

function decorate(view: EditorView): DecorationSet {
  const ranges = [];
  for (const { from, to } of view.visibleRanges) {
    // A line at a time, so a reference is never cut by the edge of the visible range.
    let pos = from;
    while (pos <= to) {
      const line = view.state.doc.lineAt(pos);
      for (const [a, b] of variableRanges(line.text))
        ranges.push(mark.range(line.from + a, line.from + b));
      pos = line.to + 1;
    }
  }
  return Decoration.set(ranges, true);
}

const plugin = ViewPlugin.fromClass(
  class {
    decorations: DecorationSet;
    constructor(view: EditorView) {
      this.decorations = decorate(view);
    }
    update(u: ViewUpdate) {
      if (u.docChanged || u.viewportChanged) this.decorations = decorate(u.view);
    }
  },
  { decorations: (v) => v.decorations },
);

// The colour is set on the span and on anything inside it, so it wins whichever way the syntax
// highlighter's own spans nest with this one.
const theme = EditorView.baseTheme({
  ".cm-var-ref, .cm-var-ref span": { color: "var(--ansi-5)", fontWeight: "600" },
});

// varHighlight is the extension that draws variable references.
export function varHighlight(): Extension {
  return [plugin, theme];
}
