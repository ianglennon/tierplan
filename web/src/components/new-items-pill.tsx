// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Button } from "@/components/ui/button";

interface NewItemsPillProps {
  // How many changes are held back. At zero the pill is not drawn.
  count: number;
  // "{n} changes waiting", already translated and plural-aware.
  label: string;
  // The button's words: "Show changes".
  action: string;
  onClick: () => void;
}

// NewItemsPill tells that a list is holding changes back so nothing moves under
// the pointer, and lets the person take them now. It is a 32px outline pill
// (44px on a phone) that stays in view. The count is announced politely when
// it appears and again only when it changes, never per row.
export function NewItemsPill({ count, label, action, onClick }: NewItemsPillProps) {
  if (count <= 0) return null;
  return (
    <div className="sticky top-2 z-20 inline-flex h-8 items-center gap-1 rounded-full border bg-background pl-3 text-sm shadow-xs max-md:h-11">
      <span role="status" aria-live="polite" className="tabular-nums">
        {label}
      </span>
      <Button
        type="button"
        variant="ghost"
        size="sm"
        className="h-7 rounded-full max-md:h-11"
        onClick={onClick}
      >
        {action}
      </Button>
    </div>
  );
}
