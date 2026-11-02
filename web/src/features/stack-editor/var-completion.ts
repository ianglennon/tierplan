// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import {
  autocompletion,
  type Completion,
  type CompletionContext,
  type CompletionResult,
} from "@codemirror/autocomplete";
import type { Extension } from "@codemirror/state";
import { EditorView } from "@codemirror/view";

// Typing "${" in a stack file offers the names of the variables the stack has (plan 07-10, STK-04,
// UI-SPEC "Variable completion"): the ones in the table and the ones the files refer to, each with
// a short phrase for where its value comes from. Accepting a name writes ${NAME}.

// A variable the editor can complete: its name, and the phrase shown beside it.
export interface VarName {
  name: string;
  detail: string;
}

// What stands before the cursor when a name is wanted: "${" and the part of the name typed so far.
const OPENING = /\$\{[A-Za-z0-9_]*$/;
// While the text typed after "${" still looks like a name, the list stays open and narrows.
const NAME_SO_FAR = /^[A-Za-z0-9_]*$/;

// acceptName writes the name over what was typed after "${" and closes the brace, unless the editor
// already has a closing brace there, in which case the cursor steps over it.
function acceptName(name: string): Completion["apply"] {
  return (view, _completion, from, to) => {
    const closed = view.state.sliceDoc(to, to + 1) === "}";
    const insert = closed ? name : `${name}}`;
    view.dispatch({
      changes: { from, to, insert },
      selection: { anchor: from + name.length + 1 },
      userEvent: "input.complete",
    });
  };
}

// completeVariableNames offers the names of variables after a "${", and nothing anywhere else. The
// names are read each time the list opens, so a variable added a moment ago is offered without the
// editor being rebuilt.
export function completeVariableNames(
  getNames: () => readonly VarName[],
): (context: CompletionContext) => CompletionResult | null {
  return (context) => {
    const before = context.matchBefore(OPENING);
    if (!before) return null;
    const names = getNames();
    if (names.length === 0) return null;
    return {
      from: before.from + 2,
      options: names.map(({ name, detail }) => ({
        label: name,
        detail,
        type: "variable",
        apply: acceptName(name),
      })),
      validFor: NAME_SO_FAR,
    };
  };
}

// The list is drawn in the colours of the app, like the problem tooltips.
const completionTheme = EditorView.theme({
  ".cm-tooltip.cm-tooltip-autocomplete": {
    backgroundColor: "var(--popover)",
    color: "var(--popover-foreground)",
    border: "1px solid var(--border)",
    borderRadius: "var(--radius-md)",
    fontFamily: "var(--font-sans)",
    fontSize: "12px",
    maxWidth: "min(480px, 90vw)",
  },
  ".cm-tooltip-autocomplete ul li": { padding: "2px 8px" },
  ".cm-tooltip-autocomplete ul li[aria-selected]": {
    backgroundColor: "var(--accent)",
    color: "var(--accent-foreground)",
  },
  ".cm-completionDetail": {
    marginLeft: "0.75em",
    color: "var(--muted-foreground)",
    fontStyle: "normal",
  },
  ".cm-completionLabel": { fontFamily: "var(--font-mono)" },
});

// varCompletion is the editor extension behind typing "${". It replaces every other source of
// completion in the editor, which has none.
export function varCompletion(getNames: () => readonly VarName[]): Extension {
  return [
    autocompletion({
      override: [completeVariableNames(getNames)],
      icons: false,
      closeOnBlur: true,
    }),
    completionTheme,
  ];
}
