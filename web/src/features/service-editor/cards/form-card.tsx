// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { type ReactNode, useCallback, useEffect, useId, useState } from "react";

import { Card, CardContent, CardHeader } from "@/components/ui/card";
import type { SpecValue } from "@/features/spec-editor/spec-format";

// What every card of the Form is given. set and unset change one path of the spec as it is now,
// never of the one the card was drawn with, so two changes in one event don't undo each other.
// Nothing else in the spec moves.
export interface CardProps {
  spec: SpecValue;
  set: (path: string, value: unknown) => void;
  // Takes the setting out of the spec, if it is there.
  unset: (path: string) => void;
  // Changes the spec as it is now by a function of it, for a change that is more than one path
  // (the auto-rollback box seeds an UpdateConfig). Nothing else in the spec moves.
  change: (fn: (spec: SpecValue) => SpecValue) => void;
  mode: "edit" | "create";
  envId: string;
  // Tells the form whether this card holds a field that can't be written. A card keeps its own
  // id for it.
  report: (card: string, invalid: boolean) => void;
}

// 44px targets under 768px (D-64), the dense 32px size from there up.
export const TOUCH_HEIGHT = "h-11 md:h-8";

// FormCard is one card of the Form: a titled region.
export function FormCard({ title, children }: { title: string; children: ReactNode }) {
  const id = useId();
  return (
    <Card role="region" aria-labelledby={id}>
      <CardHeader>
        <h2 id={id} className="text-base font-medium">
          {title}
        </h2>
      </CardHeader>
      <CardContent className="flex flex-col gap-4">{children}</CardContent>
    </Card>
  );
}

// useReport tells the form, when it changes, whether the card has a field that is not usable.
export function useReport(card: string, report: CardProps["report"], invalid: boolean): void {
  useEffect(() => {
    report(card, invalid);
  }, [card, report, invalid]);
}

// useTouched remembers which fields the person has changed. A field is told off for being wrong
// only once it has been touched, so a form opens without a page of errors.
export function useTouched(): [(field: string) => boolean, (field: string) => void] {
  const [touched, setTouched] = useState<ReadonlySet<string>>(new Set());
  const touch = useCallback(
    (field: string) => setTouched((prev) => (prev.has(field) ? prev : new Set(prev).add(field))),
    [],
  );
  return [(field) => touched.has(field), touch];
}
