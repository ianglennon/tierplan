// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Link } from "@tanstack/react-router";
import { CircleX } from "lucide-react";

import type { StackProblem } from "@/api/stacks";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { useT } from "@/i18n/use-t";

interface BlockingListProps {
  problems: StackProblem[];
  envId: string;
  stackId: string;
}

// ProblemLink is where a problem is, as a link that opens the editor on that file at that line
// (D-10). A problem with no place in a file is its message alone.
export function ProblemLink({
  problem,
  envId,
  stackId,
}: {
  problem: StackProblem;
  envId: string;
  stackId: string;
}) {
  if (!problem.file || problem.line < 1) return null;
  return (
    <Link
      to="/env/$envId/stacks/$stackId/editor"
      params={{ envId, stackId }}
      search={{ file: problem.file, line: problem.line }}
      className="rounded-sm font-mono text-xs break-all text-accent-ink underline underline-offset-3 focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
    >
      {`${problem.file}:${problem.line}`}
    </Link>
  );
}

// BlockingList is the alert that lists every error that stops the deploy, each with its file and
// line (STK-06, D-10): the person goes back to the editor at the line, with the draft as it was
// saved. The server decides what is an error, and its messages are the page's words (T-07-71).
// It draws nothing when there is no error.
export function BlockingList({ problems, envId, stackId }: BlockingListProps) {
  const t = useT();
  const errors = problems.filter((p) => p.severity === "error");
  if (errors.length === 0) return null;
  return (
    <Alert variant="destructive">
      <CircleX aria-hidden="true" />
      <AlertTitle>{t("stackReview.blocking.title", { count: errors.length })}</AlertTitle>
      <AlertDescription className="flex flex-col gap-3">
        <ul className="flex flex-col gap-1.5">
          {errors.map((p) => (
            <li
              key={`${p.file}:${p.line}:${p.col}:${p.code}:${p.message}`}
              className="flex flex-wrap items-baseline gap-x-2"
            >
              <span className="break-words">{p.message}</span>
              <ProblemLink problem={p} envId={envId} stackId={stackId} />
            </li>
          ))}
        </ul>
        <div>
          <Button asChild variant="outline" className="max-md:h-11">
            <Link to="/env/$envId/stacks/$stackId/editor" params={{ envId, stackId }}>
              {t("stackReview.blocking.back")}
            </Link>
          </Button>
        </div>
      </AlertDescription>
    </Alert>
  );
}
