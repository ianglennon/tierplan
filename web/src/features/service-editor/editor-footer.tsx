// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { ReactNode } from "react";

// EditorFooter is the 48px bar the editor's buttons sit in. It stays at the bottom of the window
// while the page scrolls, so Review changes and Apply changes are always in reach, and it keeps
// two buttons side by side under 768px (UI-SPEC "Service editor").
export function EditorFooter({ children }: { children: ReactNode }) {
  return (
    <div
      data-slot="editor-footer"
      className="sticky bottom-0 z-10 -mx-4 flex h-12 items-center justify-end gap-2 border-t bg-background px-4 max-md:h-14 md:-mx-6 md:px-6"
    >
      {children}
    </div>
  );
}
