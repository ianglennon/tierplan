// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { HighlightStyle, syntaxHighlighting } from "@codemirror/language";
import { EditorView } from "@codemirror/view";
import { tags as t } from "@lezer/highlight";

// The editor's and the viewer's colours, all drawn from variables the page already has, so both
// themes work without a new token (UI-SPEC "Color", "Code syntax colours"). Keys are the blue
// of the log palette, strings its green, numbers and booleans its yellow; comments are muted and
// punctuation is the text colour. They are code colours, not status.
export const specHighlight = syntaxHighlighting(
  HighlightStyle.define([
    { tag: [t.propertyName, t.definition(t.propertyName)], color: "var(--ansi-4)" },
    { tag: [t.string, t.special(t.string)], color: "var(--ansi-2)" },
    { tag: [t.number, t.bool, t.null, t.atom], color: "var(--ansi-3)" },
    { tag: [t.comment, t.lineComment, t.blockComment], color: "var(--muted-foreground)" },
    {
      tag: [t.punctuation, t.separator, t.bracket, t.squareBracket, t.brace, t.operator, t.meta],
      color: "var(--foreground)",
    },
  ]),
);

// The editor's frame: Geist Mono 14px at a line height of 1.5, the card's surface, line numbers
// in the label size, the 2px focus ring, and search matches at 18% of the text colour with the
// current one filled (UI-SPEC "Typography", "Color").
export const specTheme = EditorView.theme({
  "&": {
    backgroundColor: "var(--card)",
    color: "var(--foreground)",
    fontSize: "14px",
    border: "1px solid var(--border)",
    borderRadius: "var(--radius-lg)",
  },
  "&.cm-focused": { outline: "2px solid var(--ring)", outlineOffset: "0" },
  ".cm-scroller": { fontFamily: "var(--font-mono)", lineHeight: "1.5", overflow: "auto" },
  ".cm-content": { caretColor: "var(--foreground)", padding: "8px 0" },
  ".cm-cursor, .cm-dropCursor": { borderLeftColor: "var(--foreground)" },
  ".cm-gutters": {
    backgroundColor: "var(--card)",
    color: "var(--muted-foreground)",
    border: "none",
    fontSize: "12px",
  },
  ".cm-activeLine": { backgroundColor: "transparent" },
  ".cm-activeLineGutter": { backgroundColor: "transparent", color: "var(--foreground)" },
  "&.cm-focused > .cm-scroller > .cm-selectionLayer .cm-selectionBackground, .cm-selectionBackground, .cm-content ::selection":
    { backgroundColor: "color-mix(in srgb, var(--muted-foreground) 35%, transparent)" },
  ".cm-selectionMatch": {
    backgroundColor: "color-mix(in srgb, var(--foreground) 12%, transparent)",
  },
  ".cm-searchMatch": { backgroundColor: "color-mix(in srgb, var(--foreground) 18%, transparent)" },
  ".cm-searchMatch.cm-searchMatch-selected": {
    backgroundColor: "var(--foreground)",
    color: "var(--background)",
  },
  ".cm-panels": {
    backgroundColor: "var(--muted)",
    color: "var(--foreground)",
    borderColor: "var(--border)",
    fontFamily: "var(--font-sans)",
    fontSize: "14px",
  },
  ".cm-panels input, .cm-panels button": { font: "inherit" },
  ".cm-panel.cm-search input.cm-textfield": {
    backgroundColor: "var(--background)",
    color: "var(--foreground)",
    border: "1px solid var(--border)",
    borderRadius: "var(--radius-sm)",
    padding: "2px 6px",
  },
  ".cm-panel.cm-search button": {
    backgroundColor: "var(--background)",
    color: "var(--foreground)",
    border: "1px solid var(--border)",
    borderRadius: "var(--radius-sm)",
    backgroundImage: "none",
  },
});
