// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

export type SpecFormat = "yaml" | "json";

export type ParseResult =
  | { ok: true; value: Record<string, unknown> }
  | { ok: false; line: number; message: string };

export const SPEC_FORMAT_KEY = "bayplan.spec-format";

export function renderSpec(_value: unknown, _format: SpecFormat): string {
  return "";
}

export function parseSpec(_text: string, _format: SpecFormat): ParseResult {
  return { ok: false, line: 0, message: "" };
}

export function hasYamlComments(_text: string): boolean {
  return false;
}

export function loadSpecFormat(): SpecFormat {
  return "yaml";
}

export function saveSpecFormat(_format: SpecFormat): void {}
