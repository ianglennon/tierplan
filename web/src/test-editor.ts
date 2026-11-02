// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { EditorView } from "@codemirror/view";
import { act } from "@testing-library/react";

// What the tests of a page with a spec editor share: reading the text of the editor on the page
// and typing into it. jsdom does not lay a page out, so a person's typing is a change to the
// document, which is what the editor's own change handler hears.

export function editorView(): EditorView {
  const dom = document.querySelector<HTMLElement>(".cm-editor");
  const view = dom ? EditorView.findFromDOM(dom) : null;
  if (!view) throw new Error("no editor on the page");
  return view;
}

export const editorText = () => editorView().state.doc.toString();

// typeIntoEditor replaces the whole text, as one change.
export function typeIntoEditor(text: string): void {
  const view = editorView();
  act(() => {
    view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: text } });
  });
}
