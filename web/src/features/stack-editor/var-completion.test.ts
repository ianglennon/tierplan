// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import {
  CompletionContext,
  type CompletionResult,
  completionStatus,
  currentCompletions,
  startCompletion,
} from "@codemirror/autocomplete";
import { EditorState } from "@codemirror/state";
import { EditorView } from "@codemirror/view";
import { waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import {
  completeVariableNames,
  type VarName,
  varCompletion,
} from "@/features/stack-editor/var-completion";

// Typing ${ in a stack file offers the names of the variables the stack has (plan 07-10, STK-04,
// UI-SPEC "Variable completion"): the ones in the table and the ones the files refer to, each with
// where its value comes from. Accepting one writes ${NAME}.

const NAMES: VarName[] = [
  { name: "WEB_PORT", detail: "default in the file" },
  { name: "DB_PASS", detail: "from the table" },
];

// stateAt makes an editor state whose document is text with the cursor at the marker "|".
function stateAt(text: string) {
  const pos = text.indexOf("|");
  const doc = text.replace("|", "");
  return { state: EditorState.create({ doc }), pos };
}

function resultOf(text: string, explicit = false): CompletionResult | null {
  const { state, pos } = stateAt(text);
  const source = completeVariableNames(() => NAMES);
  const out = source(new CompletionContext(state, pos, explicit));
  if (out instanceof Promise) throw new Error("the source answers at once");
  return out;
}

describe("when completion opens", () => {
  it(`opens after \${ and offers every name with where its value comes from`, () => {
    const res = resultOf(`image: \${|`);
    expect(res).not.toBeNull();
    expect(res?.options.map((o) => o.label)).toEqual(["WEB_PORT", "DB_PASS"]);
    expect(res?.options.map((o) => o.detail)).toEqual(["default in the file", "from the table"]);
    expect(res?.from).toBe(`image: \${`.length);
  });

  it(`keeps what has been typed after \${ as the text to complete`, () => {
    const res = resultOf('ports: ["${WE|');
    expect(res?.from).toBe('ports: ["${'.length);
    expect(res?.validFor).toBeDefined();
  });

  it(`does not open outside a \${ context`, () => {
    for (const text of [
      "image: ngi|",
      "image: $|",
      "image: $WE|",
      "image: {|",
      "|",
      `x: \${WEB_PORT}|`,
    ]) {
      expect(resultOf(text), text).toBeNull();
    }
  });

  it(`does not open for an explicit request outside a \${ context either`, () => {
    expect(resultOf("image: ngi|", true)).toBeNull();
  });

  it("offers nothing when the stack has no variables yet", () => {
    const { state, pos } = stateAt(`\${|`);
    const out = completeVariableNames(() => [])(new CompletionContext(state, pos, false));
    expect(out === null || (out as CompletionResult).options.length === 0).toBe(true);
  });

  it("reads the names when it opens, so a new variable is offered without recreating the editor", () => {
    let names: VarName[] = [];
    const source = completeVariableNames(() => names);
    const { state, pos } = stateAt(`\${|`);
    names = [{ name: "LATE", detail: "from the table" }];
    const out = source(new CompletionContext(state, pos, false)) as CompletionResult;
    expect(out.options.map((o) => o.label)).toEqual(["LATE"]);
  });
});

describe("accepting a name", () => {
  let view: EditorView | undefined;
  afterEach(() => {
    view?.destroy();
    view = undefined;
  });

  function accept(text: string, name: string) {
    const { state, pos } = stateAt(text);
    view = new EditorView({
      state: state.update({ selection: { anchor: pos } }).state,
      parent: document.body,
    });
    const res = resultOf(text) as CompletionResult;
    const option = res.options.find((o) => o.label === name);
    if (!option || typeof option.apply !== "function")
      throw new Error("the option needs an apply function");
    option.apply(view, option, res.from, pos);
    return view;
  }

  it(`writes \${NAME} and puts the cursor after it`, () => {
    const v = accept(`image: \${|`, "WEB_PORT");
    expect(v.state.doc.toString()).toBe(`image: \${WEB_PORT}`);
    expect(v.state.selection.main.head).toBe(`image: \${WEB_PORT}`.length);
  });

  it("replaces the part already typed", () => {
    const v = accept(`image: \${WE|`, "WEB_PORT");
    expect(v.state.doc.toString()).toBe(`image: \${WEB_PORT}`);
  });

  it("does not write a second } when one is already there, and steps over it", () => {
    const v = accept(`image: \${|} # keep`, "DB_PASS");
    expect(v.state.doc.toString()).toBe(`image: \${DB_PASS} # keep`);
    expect(v.state.selection.main.head).toBe(`image: \${DB_PASS}`.length);
  });
});

describe("the extension", () => {
  it(`opens the list in an editor when \${ is typed and lists the names`, async () => {
    const { state, pos } = stateAt(`image: \${|`);
    const view = new EditorView({
      state: EditorState.create({
        doc: state.doc.toString(),
        selection: { anchor: pos },
        extensions: [varCompletion(() => NAMES)],
      }),
      parent: document.body,
    });
    try {
      startCompletion(view);
      await waitFor(() => expect(completionStatus(view.state)).toBe("active"));
      // The editor lists the names by name once nothing narrows them.
      expect(currentCompletions(view.state).map((c) => c.label)).toEqual(["DB_PASS", "WEB_PORT"]);
    } finally {
      view.destroy();
    }
  });
});
