// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

export interface SpecEditorProps {
  // The spec to start from, and what Discard edits goes back to. Hidden values are in it as
  // the server sent them.
  initial: Record<string, unknown>;
  // The parsed spec, each time the text parses.
  onChange: (value: Record<string, unknown>) => void;
  // Whether the text parses, each time that changes.
  onValidityChange?: (valid: boolean) => void;
}

export function SpecEditor(_props: SpecEditorProps) {
  return <div />;
}
