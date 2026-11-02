// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { json } from "@codemirror/lang-json";
import { yaml } from "@codemirror/lang-yaml";
import type { Extension } from "@codemirror/state";
import { EditorView } from "@codemirror/view";

import { specHighlight, specTheme } from "@/features/spec-editor/highlight";
import type { SpecFormat } from "@/features/spec-editor/spec-format";

// What the editor and the viewer share: the CodeMirror wrapper's own props and the extensions that
// make a document a labelled multiline textbox.

export type SpecLanguage = SpecFormat | "text";

// languageOf says what language a document is in: the format when the caller knows it, else
// what the file name says, else plain text. Only YAML and JSON are highlighted (UI-SPEC item 24).
export function languageOf(format?: SpecFormat, fileName?: string): SpecLanguage {
  if (format) return format;
  const name = fileName?.toLowerCase() ?? "";
  if (name.endsWith(".json")) return "json";
  if (name.endsWith(".yaml") || name.endsWith(".yml")) return "yaml";
  return "text";
}

// The wrapper's props. Tab is not captured: the wrapper binds it to indenting unless told not to,
// and a Tab that indents traps keyboard users in the editor (UI-SPEC "Spec editor keyboard").
// The wrapper's own light theme is off in favour of specTheme, its highlight fallback is off in
// favour of specHighlight, and the parts of its basic setup a spec does not use (completion,
// folding, lint, bracket pairing, extra cursors) are off.
export const editorProps = {
  theme: "none",
  indentWithTab: false,
  basicSetup: {
    lineNumbers: true,
    highlightActiveLineGutter: true,
    highlightActiveLine: false,
    foldGutter: false,
    autocompletion: false,
    closeBrackets: false,
    bracketMatching: true,
    dropCursor: false,
    rectangularSelection: false,
    crosshairCursor: false,
    syntaxHighlighting: false,
    completionKeymap: false,
    lintKeymap: false,
    foldKeymap: false,
    searchKeymap: true,
    historyKeymap: true,
    defaultKeymap: true,
    tabSize: 2,
  },
} as const;

interface ExtensionOptions {
  // The textbox's accessible name.
  label: string;
  language: SpecLanguage;
  wrap: boolean;
  readOnly: boolean;
}

function languageExtension(language: SpecLanguage): Extension {
  switch (language) {
    case "yaml":
      return yaml();
    case "json":
      return json();
    default:
      return [];
  }
}

// editorExtensions are the extensions both surfaces add: the language, the colours, line wrapping
// (on by default, UI-SPEC "long-text"), and the attributes of the textbox. The document is text
// to correct by hand, so the browser's spell checker, capitalising and correcting are off.
export function editorExtensions({
  label,
  language,
  wrap,
  readOnly,
}: ExtensionOptions): Extension[] {
  return [
    languageExtension(language),
    specTheme,
    specHighlight,
    wrap ? EditorView.lineWrapping : [],
    EditorView.contentAttributes.of({
      role: "textbox",
      "aria-label": label,
      "aria-multiline": "true",
      ...(readOnly ? { "aria-readonly": "true" } : {}),
      spellcheck: "false",
      autocapitalize: "off",
      autocorrect: "off",
    }),
  ];
}
