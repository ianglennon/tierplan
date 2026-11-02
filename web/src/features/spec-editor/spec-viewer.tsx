// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import CodeMirror from "@uiw/react-codemirror";
import { useMemo, useState } from "react";

import { Toggle } from "@/components/ui/toggle";
import { editorExtensions, editorProps, languageOf } from "@/features/spec-editor/editor-setup";
import type { SpecFormat } from "@/features/spec-editor/spec-format";
import { useT } from "@/i18n/use-t";

export interface SpecViewerProps {
  // The text to show, as the server sent it.
  text: string;
  // What the text is, for the textbox's accessible name.
  label: string;
  // The language, when the caller knows it; otherwise it is read from the file name, and a name
  // that says neither YAML nor JSON is plain text.
  format?: SpecFormat;
  fileName?: string;
  // The least height the text area takes, as a CSS length. A spec is read beside a form, so it
  // is short; a config is the page's content, so it is taller.
  minHeight?: string;
}

// SpecViewer is a spec or a config shown read-only, with the editor's look: a labelled multiline
// textbox that cannot be changed, line numbers, YAML or JSON colours, and a Wrap switch that is
// on by default. The text can still be focused, scrolled, selected and searched. It never writes
// anything anywhere.
export function SpecViewer({
  text,
  label,
  format,
  fileName,
  minHeight = "320px",
}: SpecViewerProps) {
  const t = useT();
  const [wrap, setWrap] = useState(true);
  const language = languageOf(format, fileName);
  const extensions = useMemo(
    () => editorExtensions({ label, language, wrap, readOnly: true }),
    [label, language, wrap],
  );
  return (
    <div data-testid="spec-viewer" data-language={language} className="flex flex-col gap-2">
      <div className="flex justify-end">
        <Toggle
          variant="outline"
          size="sm"
          className="max-md:h-11"
          pressed={wrap}
          onPressedChange={setWrap}
        >
          {t("specEditor.editor.wrap")}
        </Toggle>
      </div>
      <CodeMirror
        {...editorProps}
        value={text}
        readOnly
        extensions={extensions}
        minHeight={minHeight}
        maxHeight="calc(100svh - 320px)"
      />
    </div>
  );
}
