// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { SpecFormat } from "@/features/spec-editor/spec-format";

export interface SpecViewerProps {
  // The text to show.
  text: string;
  // What the text is, for its accessible name.
  label: string;
  // The language, when the caller knows it; otherwise it is read from the file name.
  format?: SpecFormat;
  fileName?: string;
}

export function SpecViewer(_props: SpecViewerProps) {
  return <div />;
}
