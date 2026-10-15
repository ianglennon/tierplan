// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import CodeMirror from "@uiw/react-codemirror";
import { Check, Copy, TriangleAlert } from "lucide-react";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import { copyText } from "@/components/copyable-code";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group";
import { editorExtensions, editorProps } from "@/features/spec-editor/editor-setup";
import {
  hasYamlComments,
  loadSpecFormat,
  parseSpec,
  renderSpec,
  type SpecFormat,
  type SpecValue,
  saveSpecFormat,
} from "@/features/spec-editor/spec-format";
import { useT } from "@/i18n/use-t";
import { toastError } from "@/lib/toast";

// How long typing must stop before a parse error is shown, so it is announced once and not as
// each letter is typed (UI-SPEC "Spec editor keyboard"). An object so a test can shorten it.
export const specEditorTiming = { errorMs: 400 };

// How long Copy spec says "Copied" before it goes back.
const COPIED_MS = 2000;

interface ShownError {
  line: number;
  message: string;
}

export interface SpecEditorProps {
  // The spec to start from, and what Discard edits goes back to. Hidden values are in it as the
  // server sent them (D-09): the editor never asks for the real one.
  initial: SpecValue;
  // The parsed spec, each time the text parses. Text that does not parse is never passed on.
  onChange: (value: SpecValue) => void;
  // Whether the text parses, each time that changes. It is told at once, so a Review button can
  // wait while the error itself waits for typing to stop.
  onValidityChange?: (valid: boolean) => void;
}

// SpecEditor is the Spec tab's editor (D-01): the whole service spec as Docker stores it, in YAML
// or JSON, as a labelled multiline textbox. The format switch converts the parsed value, never
// the text; a spec that does not parse cannot be converted, so the switch stays where it is and
// says why. A parse error shows as "Line n: message" 400 ms after typing stops. The text lives
// in memory only: the one thing stored in the browser is the format choice.
export function SpecEditor({ initial, onChange, onValidityChange }: SpecEditorProps) {
  const t = useT();
  const [doc, setDoc] = useState(() => {
    const format = loadSpecFormat();
    return { format, text: renderSpec(initial, format) };
  });
  const [shown, setShown] = useState<ShownError | null>(null);
  const [copied, setCopied] = useState(false);
  const host = useRef<HTMLDivElement>(null);
  const errorTimer = useRef<ReturnType<typeof setTimeout>>(undefined);
  const copiedTimer = useRef<ReturnType<typeof setTimeout>>(undefined);
  const valid = useRef(true);
  const { format, text } = doc;

  useEffect(
    () => () => {
      clearTimeout(errorTimer.current);
      clearTimeout(copiedTimer.current);
    },
    [],
  );

  const setValid = useCallback(
    (next: boolean) => {
      if (valid.current === next) return;
      valid.current = next;
      onValidityChange?.(next);
    },
    [onValidityChange],
  );

  // show puts an error on the page now, unless that very error is already there, so the same
  // error typed on is the same alert and is announced once.
  const show = useCallback((error: ShownError) => {
    setShown((prev) =>
      prev && prev.line === error.line && prev.message === error.message ? prev : error,
    );
  }, []);

  function onText(next: string) {
    setDoc((d) => ({ ...d, text: next }));
    clearTimeout(errorTimer.current);
    const result = parseSpec(next, format);
    if (result.ok) {
      setShown(null);
      setValid(true);
      onChange(result.value);
      return;
    }
    setValid(false);
    errorTimer.current = setTimeout(
      () => show({ line: result.line, message: result.message }),
      specEditorTiming.errorMs,
    );
  }

  function switchTo(next: SpecFormat) {
    if (next === format) return;
    const result = parseSpec(text, format);
    if (!result.ok) {
      clearTimeout(errorTimer.current);
      setValid(false);
      show({ line: result.line, message: result.message });
      return;
    }
    saveSpecFormat(next);
    setDoc({ format: next, text: renderSpec(result.value, next) });
  }

  const pristine = useMemo(() => renderSpec(initial, format), [initial, format]);

  function discard() {
    clearTimeout(errorTimer.current);
    setDoc({ format, text: pristine });
    setShown(null);
    setValid(true);
    onChange(initial);
  }

  async function onCopy() {
    clearTimeout(copiedTimer.current);
    if (await copyText(text, host.current)) {
      setCopied(true);
      copiedTimer.current = setTimeout(() => setCopied(false), COPIED_MS);
      return;
    }
    setCopied(false);
    toastError(t("env.copy.failed"));
  }

  const label = t("specEditor.editor.name", { format: t(`specEditor.editor.${format}`) });
  const extensions = useMemo(
    () => editorExtensions({ label, language: format, wrap: true, readOnly: false }),
    [label, format],
  );
  const comments = useMemo(() => format === "yaml" && hasYamlComments(text), [format, text]);

  return (
    <div ref={host} className="flex flex-col gap-3">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <ToggleGroup
          type="single"
          variant="outline"
          size="sm"
          value={format}
          aria-label={t("specEditor.editor.format")}
          onValueChange={(next) => {
            // A pressed item cannot be pressed off: one of the two always shows.
            if (next === "yaml" || next === "json") switchTo(next);
          }}
        >
          <ToggleGroupItem value="yaml" className="max-md:h-11">
            {t("specEditor.editor.yaml")}
          </ToggleGroupItem>
          <ToggleGroupItem value="json" className="max-md:h-11">
            {t("specEditor.editor.json")}
          </ToggleGroupItem>
        </ToggleGroup>
        <div className="flex flex-wrap items-center gap-2">
          <Button type="button" variant="outline" className="max-md:h-11" onClick={onCopy}>
            {copied ? <Check aria-hidden="true" /> : <Copy aria-hidden="true" />}
            {copied ? t("specEditor.editor.copied") : t("specEditor.editor.copy")}
          </Button>
          <Button
            type="button"
            variant="ghost"
            className="max-md:h-11"
            disabled={text === pristine}
            onClick={discard}
          >
            {t("specEditor.editor.discard")}
          </Button>
        </div>
      </div>
      <div data-testid="spec-editor">
        <CodeMirror
          {...editorProps}
          value={text}
          onChange={onText}
          extensions={extensions}
          minHeight="320px"
          maxHeight="calc(100svh - 320px)"
        />
      </div>
      <p className="text-sm text-muted-foreground">{t("specEditor.editor.helper")}</p>
      {comments ? (
        <p className="text-xs text-muted-foreground">{t("specEditor.editor.comments")}</p>
      ) : null}
      {shown ? (
        <Alert variant="warning">
          <TriangleAlert aria-hidden="true" />
          <AlertDescription className="text-status-warning-text">
            <p className="break-words">
              {t("specEditor.editor.error.line", { line: shown.line, message: shown.message })}
            </p>
            <p>{t("specEditor.editor.error.fix")}</p>
          </AlertDescription>
        </Alert>
      ) : null}
    </div>
  );
}
