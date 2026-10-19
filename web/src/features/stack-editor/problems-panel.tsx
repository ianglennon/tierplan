// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { ChevronRight, CircleCheck, CircleX, TriangleAlert } from "lucide-react";
import { useEffect, useRef, useState } from "react";

import type { StackProblem } from "@/api/stacks";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { Spinner } from "@/components/ui/spinner";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

// How long after the counts stop changing they are announced.
export const problemsAnnounceMs = 400;

interface ProblemsPanelProps {
  problems: StackProblem[];
  // A check is on its way to the server.
  checking: boolean;
  // The last check got no answer: the results shown are the last ones.
  failed: boolean;
  // The server has answered at least once. Until then the header has nothing to count.
  checked?: boolean;
  // Open the file at the problem's line and put the cursor there.
  onOpen: (problem: StackProblem) => void;
}

// ProblemsPanel is the list of what is wrong with the working copy (UI-SPEC "Editor"): a
// collapsible header with the counts and, open, a row for each problem with its severity as an
// icon and a word, its message and a file:line link that opens the editor there. It opens by
// itself when the first error appears and stays as the person leaves it. The counts are
// announced once per change, 400 ms after the last one; nothing is only a colour.
export function ProblemsPanel({
  problems,
  checking,
  failed,
  checked = true,
  onOpen,
}: ProblemsPanelProps) {
  const t = useT();
  const errors = problems.filter((p) => p.severity === "error").length;
  const warnings = problems.length - errors;
  const [open, setOpen] = useState(false);
  const autoOpened = useRef(false);

  // The first error opens the panel once. After that the person's choice stands.
  useEffect(() => {
    if (errors > 0 && !autoOpened.current) {
      autoOpened.current = true;
      setOpen(true);
    }
  }, [errors]);

  const counts =
    problems.length === 0
      ? t("stackProblems.counts.none")
      : t("stackProblems.counts.both", {
          errors: t("stackProblems.counts.errors", { count: errors }),
          warnings: t("stackProblems.counts.warnings", { count: warnings }),
        });

  const [announced, setAnnounced] = useState("");
  useEffect(() => {
    if (!checked) return;
    const timer = setTimeout(() => setAnnounced(counts), problemsAnnounceMs);
    return () => clearTimeout(timer);
  }, [counts, checked]);

  return (
    <Collapsible open={open} onOpenChange={setOpen} className="rounded-lg border">
      <div className="flex min-h-10 items-center gap-3 pr-3">
        <CollapsibleTrigger asChild>
          <button
            type="button"
            className="flex min-h-10 min-w-0 flex-1 items-center gap-2 px-3 text-left max-md:min-h-11"
          >
            <ChevronRight
              aria-hidden="true"
              className={cn("size-4 shrink-0 transition-transform", open && "rotate-90")}
            />
            <span className="font-medium">{t("stackProblems.title")}</span>
            {checked ? (
              <span className="flex items-center gap-1 text-sm text-muted-foreground">
                {problems.length === 0 ? (
                  <CircleCheck aria-hidden="true" className="size-4" />
                ) : null}
                {counts}
              </span>
            ) : null}
          </button>
        </CollapsibleTrigger>
        {checking ? (
          <span className="flex items-center gap-2 text-sm text-muted-foreground">
            <Spinner aria-hidden="true" role="presentation" />
            {t("stackProblems.checking")}
          </span>
        ) : null}
        {failed ? (
          <span className="text-sm text-muted-foreground">{t("stackProblems.failed")}</span>
        ) : null}
        <span role="status" aria-live="polite" className="sr-only">
          {announced}
        </span>
      </div>
      <CollapsibleContent>
        <ul className="max-h-60 overflow-y-auto border-t">
          {problems.map((p) => {
            const severity = t(`stackProblems.severity.${p.severity}`);
            const Icon = p.severity === "error" ? CircleX : TriangleAlert;
            return (
              <li key={`${p.file}:${p.line}:${p.col}:${p.code}:${p.message}`}>
                <button
                  type="button"
                  aria-label={t("stackProblems.row.name", {
                    severity,
                    message: p.message,
                    file: p.file,
                    line: p.line,
                  })}
                  onClick={() => onOpen(p)}
                  className="flex min-h-11 w-full items-start gap-2 px-3 py-2 text-left hover:bg-muted md:min-h-9"
                >
                  <Icon
                    aria-hidden="true"
                    className={cn(
                      "mt-0.5 size-4 shrink-0",
                      p.severity === "error"
                        ? "text-status-offline-text"
                        : "text-status-warning-text",
                    )}
                  />
                  <span
                    className={cn(
                      "shrink-0 font-medium",
                      p.severity === "error"
                        ? "text-status-offline-text"
                        : "text-status-warning-text",
                    )}
                  >
                    {severity}
                  </span>
                  <span className="min-w-0 flex-1 break-words">{p.message}</span>
                  <span className="shrink-0 font-mono text-sm text-muted-foreground underline">
                    {t("stackProblems.row.where", { file: p.file, line: p.line })}
                  </span>
                </button>
              </li>
            );
          })}
        </ul>
      </CollapsibleContent>
    </Collapsible>
  );
}
