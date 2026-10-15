// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Plus, RotateCcw, X } from "lucide-react";
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
  // The stored value is hidden from the person (D-09): it is not in the row, and an empty value
  // means "leave it as it is". Typing a value replaces it.
  masked?: boolean;
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
  // For rows with masked set: the placeholder of an untouched one, and the name of the button that
  // puts it back once a value has been typed over it.
  maskedPlaceholder?: string;
  reset?: (key: string) => string;
}

interface KeyValueEditorProps {
  value: readonly KeyValueRow[];
  onChange: (rows: KeyValueRow[]) => void;
  labels: KeyValueEditorLabels;
  // A message for each row that has a problem, by the row's id.
  errors?: ReadonlyMap<string, string>;
  // Told when a masked row is put back, after the rows have been changed.
  onReset?: (row: KeyValueRow) => void;
  className?: string;
}

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

// KeyValueEditor is a list of name and value pairs: labels, driver options. Rows
// repeat with a remove button each, and one button adds a row. Names and values
// are text only, never markup.
//
// A row with masked set stands for a value the server withheld (D-09): the row never holds
// it, the value box shows a placeholder, and the name can't be changed because the server
// could not keep the value for another name. An empty value is "leave it as it is"; typing
// one replaces it, and Reset puts the row back to hidden.
export function KeyValueEditor({
  value,
  onChange,
  labels,
  errors,
  onReset,
  className,
}: KeyValueEditorProps) {
  const group = useId();
  function update(id: string, patch: Partial<KeyValueRow>) {
    onChange(value.map((r) => (r.id === id ? { ...r, ...patch } : r)));
  }
  return (
    <div className={cn("flex flex-col gap-2", className)}>
      {value.map((r, i) => {
        const error = errors?.get(r.id);
        const errorId = `${group}-e${i}`;
        const hidden = r.masked === true && r.value === "";
        const typedOver = r.masked === true && r.value !== "";
        return (
          <div key={r.id} className="flex flex-col gap-1">
            <div className="flex items-center gap-2">
              <Input
                id={`${group}-k${i}`}
                aria-label={labels.key}
                aria-invalid={error ? true : undefined}
                aria-describedby={error ? errorId : undefined}
                value={r.key}
                readOnly={r.masked === true}
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
                placeholder={hidden ? labels.maskedPlaceholder : undefined}
                onChange={(e) => update(r.id, { value: e.target.value })}
                autoComplete="off"
                autoCapitalize="off"
                autoCorrect="off"
                spellCheck={false}
                className={cn("min-w-0 flex-1 font-mono", TOUCH_HEIGHT)}
              />
              {typedOver && labels.reset ? (
                <Button
                  type="button"
                  variant="ghost"
                  size="icon"
                  className="max-md:size-11"
                  aria-label={labels.reset(r.key)}
                  onClick={() => {
                    update(r.id, { value: "" });
                    onReset?.(r);
                  }}
                >
                  <RotateCcw aria-hidden="true" />
                </Button>
              ) : null}
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
            {error ? (
              <p id={errorId} className="text-xs text-destructive">
                {error}
              </p>
            ) : null}
          </div>
        );
      })}
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
