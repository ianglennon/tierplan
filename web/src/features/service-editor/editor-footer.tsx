// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { ReactNode } from "react";

// EditorFooter is the bar the editor's buttons sit in. It is at least 48px high (56px under 768px)
// and stays at the bottom of the window while the page scrolls, so Review changes and Apply
// changes are always in reach. It is one row while its contents fit and otherwise wraps: text that
// does not fit on one line beside the buttons takes its own row at the top, and buttons that do
// not fit move to the next row, so nothing is drawn over a button at any width, including beside
// the expanded sidebar (G-09-10, 07-UI-SPEC Responsive).
export function EditorFooter({ children }: { children: ReactNode }) {
  return (
    <div
      data-slot="editor-footer"
      className="sticky bottom-0 z-10 -mx-4 flex min-h-12 flex-wrap items-center justify-end gap-2 border-t bg-background px-4 py-1 max-md:min-h-14 md:-mx-6 md:px-6"
    >
      {children}
    </div>
  );
}
