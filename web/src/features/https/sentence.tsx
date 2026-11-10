// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import type { ReactNode } from "react";

// Marks where a component goes inside a translated sentence.
export const SLOT = "\u0001";

// Sentence puts a component into a translated sentence where SLOT is.
export function Sentence({ text, slot }: { text: string; slot: ReactNode }) {
  const [before = "", after = ""] = text.split(SLOT);
  return (
    <>
      {before}
      {slot}
      {after}
    </>
  );
}
