// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

interface SpecDiffProps {
  // The service the changes are to, for the table's caption.
  name: string;
  // The two specs as the manager states them, already masked by the server.
  current: unknown;
  next: unknown;
  // The headings of the two value columns; Current and New when left out.
  headings?: { current?: string; next?: string };
}

// SpecDiff is the field-level difference of two service specs: a native table listing each
// setting that changed, grouped under the cards the editor uses.
export function SpecDiff(_props: SpecDiffProps) {
  return null;
}
