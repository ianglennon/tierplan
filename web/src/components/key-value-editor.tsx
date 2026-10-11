// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Plus, X } from "lucide-react";
import { useId } from "react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { cn } from "@/lib/utils";

// One row of the editor. The id is only for React: it is stable while the row is
// typed in, and is not part of what the editor stands for.
export interface KeyValueRow {
  id: string;
  key: string;
  value: string;
}

let nextRow = 0;

// newRow is an empty row with an id no other row has.
export function newRow(key = "", value = ""): KeyValueRow {
  nextRow += 1;
  return { id: `kv-${nextRow}`, key, value };
}

// entriesOf is the map the rows stand for. A row with no name is left out and a
// name used twice keeps its last value.
export function entriesOf(rows: readonly KeyValueRow[]): Record<string, string> {
  const out: Record<string, string> = {};
  for (const r of rows) {
    const key = r.key.trim();
    if (key !== "") out[key] = r.value;
  }
  return out;
}

export interface KeyValueEditorLabels {
  // The button that adds a row: "Add label", "Add option".
  add: string;
  // Accessible names of the two inputs of every row.
  key: string;
  value: string;
  // The accessible name of a row's remove button, given the row's key.
  remove: (key: string) => string;
}

interface KeyValueEditorProps {
  value: readonly KeyValueRow[];
  onChange: (rows: KeyValueRow[]) => void;
  labels: KeyValueEditorLabels;
  className?: string;
}

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

// KeyValueEditor is a list of name and value pairs: labels, driver options. Rows
// repeat with a remove button each, and one button adds a row. Names and values
// are text only, never markup.
export function KeyValueEditor({ value, onChange, labels, className }: KeyValueEditorProps) {
  const group = useId();
  function update(id: string, patch: Partial<KeyValueRow>) {
    onChange(value.map((r) => (r.id === id ? { ...r, ...patch } : r)));
  }
  return (
    <div className={cn("flex flex-col gap-2", className)}>
      {value.map((r, i) => (
        <div key={r.id} className="flex items-center gap-2">
          <Input
            id={`${group}-k${i}`}
            aria-label={labels.key}
            value={r.key}
            onChange={(e) => update(r.id, { key: e.target.value })}
            autoComplete="off"
            autoCapitalize="off"
            autoCorrect="off"
            spellCheck={false}
            className={cn("min-w-0 flex-1 font-mono", TOUCH_HEIGHT)}
          />
          <Input
            id={`${group}-v${i}`}
            aria-label={labels.value}
            value={r.value}
            onChange={(e) => update(r.id, { value: e.target.value })}
            autoComplete="off"
            autoCapitalize="off"
            autoCorrect="off"
            spellCheck={false}
            className={cn("min-w-0 flex-1 font-mono", TOUCH_HEIGHT)}
          />
          <Button
            type="button"
            variant="ghost"
            size="icon"
            className="max-md:size-11"
            aria-label={labels.remove(r.key)}
            onClick={() => onChange(value.filter((x) => x.id !== r.id))}
          >
            <X aria-hidden="true" />
          </Button>
        </div>
      ))}
      <div>
        <Button
          type="button"
          variant="outline"
          size="sm"
          className="max-md:h-11"
          onClick={() => onChange([...value, newRow()])}
        >
          <Plus aria-hidden="true" />
          {labels.add}
        </Button>
      </div>
    </div>
  );
}
