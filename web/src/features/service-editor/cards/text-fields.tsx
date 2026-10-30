// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useEffect, useRef, useState } from "react";

import { Field, FieldDescription, FieldError, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { TOUCH_HEIGHT } from "@/features/service-editor/cards/form-card";
import { formatDuration, parseDuration } from "@/features/service-editor/durations";
import { wholeNumber } from "@/features/service-editor/field-values";
import { getPath } from "@/features/service-editor/spec-patch";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

// The text settings of the policy cards: a duration (stored as nanoseconds), a whole number or a
// ratio from 0 to 1, each one path of the spec. Empty is the setting left out.

export type SettingKind = "duration" | "whole" | "ratio";

const RATIO = /^(\d+(\.\d*)?|\.\d+)$/;

// parseSetting reads a field's text: the number it stands for, undefined for empty text, and
// null for text that is not a value of the kind.
export function parseSetting(kind: SettingKind, text: string): number | undefined | null {
  if (kind === "duration") return parseDuration(text);
  const t = text.trim();
  if (t === "") return undefined;
  if (kind === "whole") return wholeNumber(t);
  if (!RATIO.test(t)) return null;
  const n = Number(t);
  return n >= 0 && n <= 1 ? n : null;
}

// textOf is a value of the spec as the field shows it.
export function textOf(kind: SettingKind, value: unknown): string {
  if (typeof value !== "number" || !Number.isFinite(value)) return "";
  return kind === "duration" ? formatDuration(value) : String(value);
}

export interface Setting {
  path: string;
  kind: SettingKind;
  text: string;
  // The text is not a value of the kind: it is told off and not written.
  bad: boolean;
  onChange: (text: string) => void;
}

// useSetting holds the text of one field. A change that reads as a value is written to the path,
// empty text takes the setting out, and text that is not a value is kept in the field and not
// written. When the spec changes under the field (the auto-rollback box seeds the update policy)
// and the text no longer says what the spec says, the text follows the spec.
export function useSetting(
  spec: unknown,
  path: string,
  kind: SettingKind,
  write: (path: string, value: number | undefined) => void,
): Setting {
  const value = getPath(spec, path);
  const [text, setText] = useState(() => textOf(kind, value));
  const shown = useRef(text);
  shown.current = text;
  useEffect(() => {
    const parsed = parseSetting(kind, shown.current);
    if (parsed === null) return;
    if (parsed !== (typeof value === "number" ? value : undefined)) setText(textOf(kind, value));
  }, [kind, value]);
  return {
    path,
    kind,
    text,
    bad: parseSetting(kind, text) === null,
    onChange(next) {
      setText(next);
      const parsed = parseSetting(kind, next);
      if (parsed !== null) write(path, parsed);
    },
  };
}

const ERROR: Record<SettingKind, string> = {
  duration: "serviceForm.errors.duration",
  whole: "serviceForm.errors.whole",
  ratio: "serviceForm.errors.ratio",
};

interface SettingFieldProps {
  id: string;
  label: string;
  setting: Setting;
  placeholder?: string;
  helper?: string;
}

// SettingField is a labelled text input for a Setting, with its helper and, when the text is not
// usable, the error under it.
export function SettingField({ id, label, setting, placeholder, helper }: SettingFieldProps) {
  const t = useT();
  const { bad } = setting;
  const describedBy = [helper ? `${id}-helper` : "", bad ? `${id}-error` : ""]
    .filter(Boolean)
    .join(" ");
  return (
    <Field data-invalid={bad ? true : undefined}>
      <FieldLabel htmlFor={id}>{label}</FieldLabel>
      <Input
        id={id}
        value={setting.text}
        placeholder={placeholder}
        inputMode={setting.kind === "duration" ? "text" : "decimal"}
        onChange={(e) => setting.onChange(e.target.value)}
        autoComplete="off"
        aria-invalid={bad ? true : undefined}
        aria-describedby={describedBy || undefined}
        className={cn("font-mono", TOUCH_HEIGHT)}
      />
      {helper ? <FieldDescription id={`${id}-helper`}>{helper}</FieldDescription> : null}
      {bad ? <FieldError id={`${id}-error`}>{t(ERROR[setting.kind])}</FieldError> : null}
    </Field>
  );
}
