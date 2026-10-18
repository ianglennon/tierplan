// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Eye, EyeOff } from "lucide-react";
import { useEffect, useId, useMemo, useRef, useState } from "react";

import { Button } from "@/components/ui/button";
import { Field, FieldDescription, FieldError, FieldLabel } from "@/components/ui/field";
import {
  InputGroup,
  InputGroupAddon,
  InputGroupButton,
  InputGroupInput,
} from "@/components/ui/input-group";
import { Textarea } from "@/components/ui/textarea";
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group";
import { useT } from "@/i18n/use-t";

// What a value is for: a secret (typed in a masked field, never previewed) or a config.
export type ObjectKind = "secret" | "config";

// A file the person chose. The bytes are null when the file is over the limit: it is not read.
export interface PickedFile {
  name: string;
  size: number;
  bytes: Uint8Array | null;
}

// The value of a secret or a config as a form holds it: typed text, or a file. It lives in React
// state only: never in storage, the address, a toast or the query cache (T-06-113).
export interface ObjectValue {
  source: "type" | "file";
  text: string;
  file: PickedFile | null;
  // True when the text was read from a file and has not been edited since: a config file that is
  // valid UTF-8 fills the text area, and the audit log still says where it came from.
  fromFile?: boolean;
}

export const EMPTY_VALUE: ObjectValue = { source: "type", text: "", file: null };

// How much a secret and a config may hold, in KB (D-40).
export const MAX_KB: Record<ObjectKind, number> = { secret: 500, config: 1000 };

// The most bytes a value may be: the server refuses 500 KB (512,000 bytes) and more, so one byte
// under it is the largest it takes.
export function maxBytes(kind: ObjectKind): number {
  return MAX_KB[kind] * 1024 - 1;
}

// valueBytes is what would be sent: the typed text as UTF-8, or the file's bytes.
export function valueBytes(value: ObjectValue): Uint8Array | null {
  if (value.source === "type") return new TextEncoder().encode(value.text);
  return value.file?.bytes ?? null;
}

// valueSize is how many bytes the value is.
export function valueSize(value: ObjectValue): number {
  if (value.source === "type") return new TextEncoder().encode(value.text).length;
  return value.file?.size ?? 0;
}

export type ValueProblem = "empty" | "noFile" | "tooBig";

// valueProblem says what is wrong with a value before it is sent, or undefined when it is fine.
export function valueProblem(kind: ObjectKind, value: ObjectValue): ValueProblem | undefined {
  if (value.source === "file" && value.file === null) return "noFile";
  if (value.source === "file" && value.file?.bytes === null) return "tooBig";
  const size = valueSize(value);
  if (size === 0) return "empty";
  if (size > maxBytes(kind)) return "tooBig";
  return undefined;
}

// toBase64 is the standard base64 of bytes, built in pieces so a large value does not overflow
// the call stack.
export function toBase64(bytes: Uint8Array): string {
  const piece = 0x8000;
  let binary = "";
  for (let at = 0; at < bytes.length; at += piece) {
    binary += String.fromCharCode(...bytes.subarray(at, at + piece));
  }
  return btoa(binary);
}

// kbOf is a size in KB, to one decimal and without a trailing ".0".
function kbOf(bytes: number): number {
  return Math.round((bytes / 1024) * 10) / 10;
}

// formatSize words a size in bytes: "12 B", "2 KB", "1.5 KB".
export function formatSize(bytes: number): string {
  return bytes < 1024 ? `${bytes} B` : `${kbOf(bytes)} KB`;
}

// textOf is the text of bytes that are valid UTF-8, else null. A byte order mark is kept, so the
// text is exactly the file and encodes back to the same bytes.
function textOf(bytes: Uint8Array): string | null {
  try {
    return new TextDecoder("utf-8", { fatal: true, ignoreBOM: true }).decode(bytes);
  } catch {
    return null;
  }
}

// readBytes reads a chosen file into memory with the File API. Nothing is uploaded: the bytes wait
// in state until the form is submitted.
function readBytes(file: File): Promise<Uint8Array> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => resolve(new Uint8Array(reader.result as ArrayBuffer));
    reader.onerror = () => reject(reader.error);
    reader.readAsArrayBuffer(file);
  });
}

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

interface ValueSourceFieldProps {
  kind: ObjectKind;
  value: ObjectValue;
  onChange: (value: ObjectValue) => void;
  // The problem a submit found, to show under the value.
  problem?: ValueProblem;
  disabled?: boolean;
}

// ValueSourceField is the Value source toggle (Type it, Upload a file) and the Value under it,
// shared by the create sheet, the rotate dialog and the staged objects. A secret is typed in a
// masked input that no browser fills in or checks, with a show and hide button; a file is read in
// the browser and never previewed; a config is typed in a text area. The size is shown against the
// limit as it grows. The value is handed to the owner only, and the field hands the owner an empty
// value when it goes away, so nothing outlives it.
export function ValueSourceField({
  kind,
  value,
  onChange,
  problem,
  disabled,
}: ValueSourceFieldProps) {
  const t = useT();
  const id = useId();
  const chooser = useRef<HTMLInputElement>(null);
  const [shown, setShown] = useState(false);
  const [readFailed, setReadFailed] = useState(false);
  const max = MAX_KB[kind];

  // The latest onChange, so the clean-up on unmount calls the owner's current one.
  const latest = useRef(onChange);
  latest.current = onChange;
  const alive = useRef(true);
  useEffect(() => {
    alive.current = true;
    return () => {
      alive.current = false;
      latest.current(EMPTY_VALUE);
    };
  }, []);

  const size = useMemo(() => valueSize(value), [value]);
  // Too big shows as soon as it is true; the rest wait for a submit to say them.
  const live = size > maxBytes(kind) || value.file?.bytes === null ? "tooBig" : undefined;
  const shownProblem = live ?? problem;
  const errorId = `${id}-error`;
  const helperId = `${id}-helper`;

  function setSource(next: string) {
    // Pressing the chosen one again would leave none chosen.
    if (next !== "type" && next !== "file") return;
    if (next === value.source) return;
    setShown(false);
    setReadFailed(false);
    onChange({ source: next, text: "", file: null });
  }

  async function choose(file: File | undefined) {
    if (!file) return;
    setReadFailed(false);
    if (file.size > maxBytes(kind)) {
      onChange({
        source: "file",
        text: "",
        file: { name: file.name, size: file.size, bytes: null },
      });
      return;
    }
    try {
      const bytes = await readBytes(file);
      if (!alive.current) return;
      // A config that is text fills the text area, to be read and edited; one that is not stays
      // an opaque file with its name and size. A secret is never shown, so it stays a file.
      const text = kind === "config" ? textOf(bytes) : null;
      if (text !== null) onChange({ source: "type", text, file: null, fromFile: true });
      else
        onChange({ source: "file", text: "", file: { name: file.name, size: file.size, bytes } });
    } catch {
      if (alive.current) {
        setReadFailed(true);
        onChange({ source: "file", text: "", file: null });
      }
    }
  }

  const sizeLine = (
    <p className="text-xs text-muted-foreground tabular-nums">
      {t("secrets.value.size", { used: kbOf(size), max })}
    </p>
  );
  const errorText = readFailed
    ? t("secrets.value.readFailed")
    : shownProblem === "tooBig"
      ? t(kind === "config" ? "configs.value.tooBig" : "secrets.value.tooBig", { max })
      : shownProblem
        ? t(`secrets.value.${shownProblem}`)
        : undefined;
  const error = errorText ? <FieldError id={errorId}>{errorText}</FieldError> : null;

  return (
    <div className="flex flex-col gap-4">
      <Field>
        <FieldLabel id={`${id}-source`}>{t("secrets.value.source.label")}</FieldLabel>
        <ToggleGroup
          type="single"
          variant="outline"
          size="sm"
          spacing={0}
          aria-labelledby={`${id}-source`}
          value={value.source}
          disabled={disabled}
          onValueChange={setSource}
          className="w-fit"
        >
          <ToggleGroupItem value="type" className="max-md:h-11">
            {t("secrets.value.source.type")}
          </ToggleGroupItem>
          <ToggleGroupItem value="file" className="max-md:h-11">
            {t("secrets.value.source.file")}
          </ToggleGroupItem>
        </ToggleGroup>
      </Field>

      {value.source === "type" && kind === "secret" ? (
        <Field data-invalid={errorText ? true : undefined}>
          <FieldLabel htmlFor={`${id}-value`}>{t("secrets.value.label")}</FieldLabel>
          <InputGroup className="max-md:h-11">
            <InputGroupInput
              id={`${id}-value`}
              type={shown ? "text" : "password"}
              autoComplete="new-password"
              autoCapitalize="off"
              autoCorrect="off"
              spellCheck={false}
              disabled={disabled}
              aria-invalid={errorText ? true : undefined}
              aria-describedby={errorText ? `${helperId} ${errorId}` : helperId}
              value={value.text}
              onChange={(e) => onChange({ source: "type", text: e.target.value, file: null })}
            />
            <InputGroupAddon align="inline-end">
              <InputGroupButton
                size="icon-xs"
                className="max-md:size-11"
                aria-pressed={shown}
                aria-label={t(shown ? "secrets.value.hide" : "secrets.value.show")}
                onClick={() => setShown((s) => !s)}
              >
                {shown ? <EyeOff aria-hidden="true" /> : <Eye aria-hidden="true" />}
              </InputGroupButton>
            </InputGroupAddon>
          </InputGroup>
          <FieldDescription id={helperId}>{t("secrets.value.helper")}</FieldDescription>
          {sizeLine}
          {error}
        </Field>
      ) : null}

      {value.source === "type" && kind === "config" ? (
        <Field data-invalid={errorText ? true : undefined}>
          <FieldLabel htmlFor={`${id}-value`}>{t("secrets.value.contentLabel")}</FieldLabel>
          <Textarea
            id={`${id}-value`}
            rows={12}
            autoComplete="off"
            autoCapitalize="off"
            autoCorrect="off"
            spellCheck={false}
            disabled={disabled}
            aria-invalid={errorText ? true : undefined}
            aria-describedby={errorText ? errorId : undefined}
            value={value.text}
            onChange={(e) => onChange({ source: "type", text: e.target.value, file: null })}
            className="font-mono text-xs"
          />
          {sizeLine}
          {error}
        </Field>
      ) : null}

      {value.source === "file" ? (
        <Field data-invalid={errorText ? true : undefined}>
          {value.file === null ? (
            <>
              <input
                ref={chooser}
                type="file"
                tabIndex={-1}
                aria-hidden="true"
                className="sr-only"
                disabled={disabled}
                onChange={(e) => {
                  const file = e.target.files?.[0];
                  // The same file can be chosen again after it was removed.
                  e.target.value = "";
                  void choose(file);
                }}
              />
              <div>
                <Button
                  type="button"
                  variant="outline"
                  className={TOUCH_HEIGHT}
                  disabled={disabled}
                  aria-describedby={errorText ? errorId : undefined}
                  onClick={() => chooser.current?.click()}
                >
                  {t("secrets.value.choose")}
                </Button>
              </div>
            </>
          ) : (
            <div className="flex flex-wrap items-center gap-2">
              <span className="min-w-0 font-mono text-xs break-all">
                {t("secrets.value.picked", {
                  name: value.file.name,
                  size: formatSize(value.file.size),
                })}
              </span>
              <Button
                type="button"
                variant="ghost"
                size="sm"
                className="max-md:h-11"
                disabled={disabled}
                onClick={() => onChange({ source: "file", text: "", file: null })}
              >
                {t("secrets.value.remove")}
              </Button>
            </div>
          )}
          {sizeLine}
          {error}
        </Field>
      ) : null}
    </div>
  );
}
