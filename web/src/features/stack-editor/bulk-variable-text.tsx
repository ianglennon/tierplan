// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { type Ref, useEffect, useId, useImperativeHandle, useRef, useState } from "react";

import { Textarea } from "@/components/ui/textarea";
import { HIDDEN } from "@/features/spec-editor/diff";
import { parseDotenv } from "@/features/stack-editor/dotenv";
import type { WorkingVar } from "@/features/stack-editor/working-copy";
import { useT } from "@/i18n/use-t";

export type BulkParse = { ok: true; rows: WorkingVar[] } | { ok: false; line: number };

const QUOTE_ESCAPES: Record<string, string> = {
  "\\": "\\\\",
  '"': '\\"',
  "\n": "\\n",
  "\r": "\\r",
  "\t": "\\t",
};

// A value goes on its line as it is, unless a line could not carry it: a line break, a space at
// either end, a quote at the start (it would be read as quoting), or " #" (it would start a
// comment). Those are written in double quotes with their escapes, as a .env file does.
function needsQuotes(value: string): boolean {
  return (
    /[\r\n]/.test(value) || value !== value.trim() || /^["']/.test(value) || /[ \t]#/.test(value)
  );
}

function quoted(value: string): string {
  return `"${value.replace(/[\\"\n\r\t]/g, (c) => QUOTE_ESCAPES[c] as string)}"`;
}

// toBulkText is the table as text: one NAME=value line per row. A secret row is NAME=<hidden>
// whether its value is saved or typed, so the text never holds a secret value (STK-04). The line
// is the whole of what a secret shows: leaving it alone keeps the value.
export function toBulkText(rows: readonly WorkingVar[]): string {
  return rows
    .map((r) => {
      if (r.secret) return `${r.name}=${HIDDEN}`;
      return `${r.name}=${needsQuotes(r.value) ? quoted(r.value) : r.value}`;
    })
    .join("\n");
}

// fromBulkText reads the text back into rows. A comment or a blank line is dropped; a line that
// is not NAME=value is a failure that names the first such line (1-based). A line for a secret
// that still says <hidden> is that row exactly as it was; any other value replaces it and the
// row stays a secret. A name the text does not have is removed, and a name that is new is a plain
// variable: which rows are secret is set in the table.
export function fromBulkText(text: string, rows: readonly WorkingVar[]): BulkParse {
  const parsed = parseDotenv(text);
  const bad = parsed.ignored.find((l) => l.reason === "malformed");
  if (bad) return { ok: false, line: bad.line };
  const was = new Map(rows.map((r) => [r.name, r]));
  const out = parsed.vars.map(({ name, value }): WorkingVar => {
    const row = was.get(name);
    if (value === HIDDEN && row?.secret) return row;
    if (row) return { ...row, value, hidden: false };
    return { name, value, secret: false, hidden: false };
  });
  out.sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
  return { ok: true, rows: out };
}

// What the pane can ask of the text. leave() is true when the text parses (or has nothing to
// say); false when it does not, and then the message is shown and has the focus.
export interface BulkVariableTextHandle {
  leave: () => boolean;
}

export interface BulkVariableTextProps {
  ref?: Ref<BulkVariableTextHandle>;
  // The rows of the working copy now.
  rows: readonly WorkingVar[];
  // The text parsed into rows: the working copy becomes them.
  onApply: (rows: WorkingVar[]) => void;
}

// BulkVariableText is "Bulk edit as text" (D-04): the table as a textarea of NAME=value lines.
// The text lives in this component's state only and holds no secret value. Each time the text
// parses, the working copy is brought in step with it, so Save draft and the check never miss an
// edit; while it does not parse, the working copy keeps the last text that did, and the message
// appears when the person leaves the box or tries to go back to the table.
export function BulkVariableText({ ref, rows, onApply }: BulkVariableTextProps) {
  const t = useT();
  const ids = useId();
  const message = useRef<HTMLParagraphElement>(null);
  // The rows as the text was opened on: a <hidden> line finds its secret here when the line was
  // deleted and written back, because by then the working copy no longer has the row.
  const opened = useRef(rows);
  const [text, setText] = useState(() => toBulkText(rows));
  const [shown, setShown] = useState(false);
  const [focusTick, setFocusTick] = useState(0);

  const known = [...rows, ...opened.current.filter((o) => !rows.some((r) => r.name === o.name))];
  const parsed = fromBulkText(text, known);
  const failure = parsed.ok ? null : parsed.line;

  useEffect(() => {
    if (focusTick > 0) message.current?.focus();
  }, [focusTick]);

  useImperativeHandle(ref, () => ({
    leave: () => {
      if (parsed.ok) return true;
      setShown(true);
      setFocusTick((n) => n + 1);
      return false;
    },
  }));

  function change(value: string) {
    setText(value);
    setShown(false);
    const next = fromBulkText(value, known);
    if (next.ok) onApply(next.rows);
  }

  const showError = shown && failure !== null;
  const describedBy = [`${ids}-help`, showError ? `${ids}-error` : null].filter(Boolean).join(" ");
  return (
    <div className="flex flex-col gap-2">
      <Textarea
        value={text}
        onChange={(e) => change(e.target.value)}
        onBlur={() => setShown(true)}
        rows={12}
        aria-label={t("stackImport.bulk.label")}
        aria-invalid={showError ? true : undefined}
        aria-describedby={describedBy}
        autoComplete="off"
        autoCapitalize="off"
        autoCorrect="off"
        spellCheck={false}
        className="max-h-96 min-h-0 field-sizing-fixed overflow-y-auto font-mono break-all md:text-xs"
      />
      <p id={`${ids}-help`} className="text-xs text-muted-foreground">
        {t("stackImport.bulk.helper")}
      </p>
      {showError ? (
        <p
          ref={message}
          id={`${ids}-error`}
          role="alert"
          tabIndex={-1}
          className="rounded-sm text-sm text-destructive focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
        >
          {t("stackImport.bulk.error", { line: failure })}
        </p>
      ) : null}
    </div>
  );
}
