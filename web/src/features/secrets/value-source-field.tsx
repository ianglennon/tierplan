// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// What a secret or a config holds: a secret or a config.
export type ObjectKind = "secret" | "config";

// A file the person chose. The bytes are null when the file is over the limit: it is not read.
export interface PickedFile {
  name: string;
  size: number;
  bytes: Uint8Array | null;
}

// The value of a secret or a config as the form holds it: typed text, or a file. It lives in React
// state only.
export interface ObjectValue {
  source: "type" | "file";
  text: string;
  file: PickedFile | null;
}

export const EMPTY_VALUE: ObjectValue = { source: "type", text: "", file: null };

// How much a secret and a config may hold, in KB (D-40). Not built yet.
export const MAX_KB: Record<ObjectKind, number> = { secret: 0, config: 0 };

// The most bytes a value may be: the server refuses 500 KB (512,000 bytes) and more.
export function maxBytes(kind: ObjectKind): number {
  return MAX_KB[kind];
}

// valueBytes is what would be sent: the typed text as UTF-8, or the file's bytes.
export function valueBytes(_value: ObjectValue): Uint8Array | null {
  return null;
}

// valueSize is how many bytes the value is.
export function valueSize(_value: ObjectValue): number {
  return -1;
}

// valueProblem says what is wrong with a value before it is sent, or undefined when it is fine.
export function valueProblem(
  _kind: ObjectKind,
  _value: ObjectValue,
): "empty" | "noFile" | "tooBig" | undefined {
  return undefined;
}

// formatSize words a size in bytes: "12 B", "2 KB", "1.5 KB".
export function formatSize(_bytes: number): string {
  return "";
}

interface ValueSourceFieldProps {
  kind: ObjectKind;
  value: ObjectValue;
  onChange: (value: ObjectValue) => void;
  // The problem to show, from a submit that found one.
  problem?: "empty" | "noFile" | "tooBig";
  disabled?: boolean;
}

// ValueSourceField is the Value source toggle and the Value under it. Not built yet.
export function ValueSourceField(_props: ValueSourceFieldProps): null {
  return null;
}
