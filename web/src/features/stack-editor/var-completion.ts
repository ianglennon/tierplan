// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { CompletionContext, CompletionResult } from "@codemirror/autocomplete";
import type { Extension } from "@codemirror/state";

// A variable the editor can complete: its name, and a short phrase for where its value comes from.
export interface VarName {
  name: string;
  detail: string;
}

// varCompletionSource offers the names of variables after a "${".
export function varCompletionSource(
  _getNames: () => readonly VarName[],
): (context: CompletionContext) => CompletionResult | null {
  return () => null;
}

// varCompletion is the editor extension behind typing "${" (UI-SPEC "Variable completion").
export function varCompletion(_getNames: () => readonly VarName[]): Extension {
  return [];
}
