// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import CodeMirror from "@uiw/react-codemirror";
import { useMemo } from "react";

import { editorExtensions, editorProps, languageOf } from "@/features/spec-editor/editor-setup";
import type { WorkingFile } from "@/features/stack-editor/working-copy";
import { useT } from "@/i18n/use-t";

interface FileEditorProps {
  file: WorkingFile;
  readOnly: boolean;
  onChange: (content: string) => void;
}

// FileEditor is one stack file in a CodeMirror surface: a labelled multiline textbox, with YAML
// highlighting for a .yaml or .yml file and plain text for any other. Lines do not wrap, so a long
// line scrolls inside the surface (UI-SPEC "Overflow"), and the surface renders only what is in
// view, which keeps a 5,000-line compose file editable. Tab leaves the editor (editorProps).
// The editor is keyed by the path by its parent, so each file has its own undo history.
export function FileEditor({ file, readOnly, onChange }: FileEditorProps) {
  const t = useT();
  const language = languageOf(undefined, file.path) === "yaml" ? "yaml" : "text";
  const label = t("stackEditor.editor.name", {
    file: file.path,
    kind: t(`stackEditor.editor.kind.${language}`),
  });
  const extensions = useMemo(
    () => editorExtensions({ label, language, wrap: false, readOnly }),
    [label, language, readOnly],
  );
  return (
    <div data-testid="stack-file-editor">
      <CodeMirror
        {...editorProps}
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
