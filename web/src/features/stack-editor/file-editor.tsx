// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { EditorView } from "@codemirror/view";
import CodeMirror, { type ReactCodeMirrorRef } from "@uiw/react-codemirror";
import { type Ref, useEffect, useImperativeHandle, useMemo, useRef } from "react";

import { editorExtensions, editorProps, languageOf } from "@/features/spec-editor/editor-setup";
import {
  type LintProblem,
  lintMarkers,
  setLintProblems,
} from "@/features/stack-editor/lint-markers";
import { type VarName, varCompletion } from "@/features/stack-editor/var-completion";
import { varHighlight } from "@/features/stack-editor/var-highlight";
import { COMPOSE_FILE, type WorkingFile } from "@/features/stack-editor/working-copy";
import { useT } from "@/i18n/use-t";

// What a parent can ask of the editor: put the cursor at a place and take the focus there.
export interface FileEditorHandle {
  // cursorTo moves the cursor to a 1-based line and column, scrolls it into view and focuses the
  // editor. A line past the end goes to the last line, a column past the end to the end of the line.
  cursorTo: (line: number, col: number) => void;
}

// moveTo puts the cursor of a view at a 1-based line and column and brings it into view.
function moveTo(view: EditorView, line: number, col: number) {
  const doc = view.state.doc;
  const at = doc.line(Math.min(Math.max(line, 1), doc.lines));
  const pos = Math.min(at.from + Math.max(col - 1, 0), at.to);
  view.dispatch({
    selection: { anchor: pos },
    effects: EditorView.scrollIntoView(pos, { y: "center" }),
  });
  view.focus();
}

const NO_PROBLEMS: readonly LintProblem[] = [];
const NO_NAMES: readonly VarName[] = [];

interface FileEditorProps {
  file: WorkingFile;
  readOnly: boolean;
  // The problems of this file, drawn in the gutter and on the lines (plan 07-08). A new list
  // replaces the old without recreating the editor.
  problems?: readonly LintProblem[];
  // The variables typing "${" offers (plan 07-10). Only compose.yaml has them: it is the one file
  // Bayplan interpolates. A new list is used the next time the list opens; the editor is not
  // recreated.
  variableNames?: readonly VarName[];
  onChange: (content: string) => void;
  ref?: Ref<FileEditorHandle>;
}

// FileEditor is one stack file in a CodeMirror surface: a labelled multiline textbox, with YAML
// highlighting for a .yaml or .yml file and plain text for any other. Lines do not wrap, so a long
// line scrolls inside the surface (UI-SPEC "Overflow"), and the surface renders only what is in
// view, which keeps a 5,000-line compose file editable. Tab leaves the editor (editorProps).
// The editor is keyed by the path by its parent, so each file has its own undo history.
export function FileEditor({
  file,
  readOnly,
  problems = NO_PROBLEMS,
  variableNames = NO_NAMES,
  onChange,
  ref,
}: FileEditorProps) {
  const t = useT();
  const cm = useRef<ReactCodeMirrorRef>(null);
  const language = languageOf(undefined, file.path) === "yaml" ? "yaml" : "text";
  const label = t("stackEditor.editor.name", {
    file: file.path,
    kind: t(`stackEditor.editor.kind.${language}`),
  });
  const names = useRef(variableNames);
  names.current = variableNames;
  const completes = file.path === COMPOSE_FILE && !readOnly;
  const extensions = useMemo(
    () => [
      ...editorExtensions({ label, language, wrap: false, readOnly }),
      ...(language === "yaml" ? [varHighlight()] : []),
      ...(completes ? [varCompletion(() => names.current)] : []),
      lintMarkers(),
    ],
    [label, language, readOnly, completes],
  );
  // The problems drawn now, and the ones to draw when the surface is created.
  const shown = useRef(problems);
  shown.current = problems;
  useEffect(() => {
    cm.current?.view?.dispatch({ effects: setLintProblems.of(problems) });
  }, [problems]);
  // A jump asked for before the surface exists (the file was only just opened) waits for it. The
  // view is kept from onCreateEditor as well as read from the ref: the library's ref follows the
  // view a render later, so a jump asked for in between would otherwise wait for a creation that
  // has already happened.
  const pending = useRef<{ line: number; col: number } | null>(null);
  const created = useRef<EditorView | null>(null);
  useImperativeHandle(
    ref,
    () => ({
      cursorTo(line, col) {
        const view = created.current ?? cm.current?.view;
        if (view) moveTo(view, line, col);
        else pending.current = { line, col };
      },
    }),
    [],
  );
  return (
    <div data-testid="stack-file-editor">
      <CodeMirror
        {...editorProps}
        ref={cm}
        onCreateEditor={(view) => {
          created.current = view;
          view.dispatch({ effects: setLintProblems.of(shown.current) });
          const jump = pending.current;
          pending.current = null;
          if (jump) moveTo(view, jump.line, jump.col);
        }}
        value={file.content}
        readOnly={readOnly}
        onChange={onChange}
        extensions={extensions}
        minHeight="400px"
        maxHeight="calc(100svh - 336px)"
      />
    </div>
  );
}
