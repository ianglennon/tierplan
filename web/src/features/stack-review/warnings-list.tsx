// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { ChevronRight } from "lucide-react";
import { useState } from "react";

import type { StackProblem } from "@/api/stacks";
import { Card, CardContent, CardHeader } from "@/components/ui/card";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { ProblemLink } from "@/features/stack-review/blocking-list";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

// The card is open by default when this many warnings or fewer are listed (UI-SPEC "Review").
const OPEN_BY_DEFAULT = 5;

interface WarningsListProps {
  problems: StackProblem[];
  envId: string;
  stackId: string;
}

// WarningsList is the collapsible card of the check's warnings (D-10). A warning informs and never
// stops a deploy, so the card is not an alert and Deploy stack does not look at it. It draws
// nothing when there is no warning.
export function WarningsList({ problems, envId, stackId }: WarningsListProps) {
  const t = useT();
  const warnings = problems.filter((p) => p.severity === "warning");
  const [open, setOpen] = useState(warnings.length <= OPEN_BY_DEFAULT);
  if (warnings.length === 0) return null;
  return (
    <Card className="gap-3 [--card-spacing:--spacing(4)] md:[--card-spacing:--spacing(6)]">
      <Collapsible open={open} onOpenChange={setOpen}>
        <CardHeader>
          <h3 className="text-base font-semibold">
            <CollapsibleTrigger className="flex items-center gap-2 rounded-sm text-start focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none max-md:min-h-11">
              <ChevronRight
                aria-hidden="true"
                className={cn("size-4 shrink-0 transition-transform", open && "rotate-90")}
              />
              {t("stackReview.warnings.title", { count: warnings.length })}
            </CollapsibleTrigger>
          </h3>
        </CardHeader>
        <CollapsibleContent>
          <CardContent>
            <ul className="flex flex-col gap-1.5 pt-3 text-sm">
              {warnings.map((p) => (
                <li
                  key={`${p.file}:${p.line}:${p.col}:${p.code}:${p.message}`}
                  className="flex flex-wrap items-baseline gap-x-2"
                >
                  <span className="break-words">{p.message}</span>
                  <ProblemLink problem={p} envId={envId} stackId={stackId} />
                </li>
              ))}
            </ul>
          </CardContent>
        </CollapsibleContent>
      </Collapsible>
    </Card>
  );
}
