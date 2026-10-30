// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { ChevronRight } from "lucide-react";
import { useState } from "react";

import type { GitTestResult, GitTestStepState } from "@/api/git";
import { StatusDot } from "@/components/status-dot";
import { Button } from "@/components/ui/button";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { keyExists, useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

const DOT = {
  passed: "online",
  failed: "offline",
  not_checked: "unknown",
  not_needed: "unknown",
} as const satisfies Record<GitTestStepState, "online" | "offline" | "unknown">;

// useFailureReason words a failure code in the copy of UI-SPEC "Shared refusals and failures".
// The server sends a code and a host and never any text from the git host or the network, so
// nothing the host said can reach the page. A code this version does not know gets the general line.
export function useFailureReason(): (failure: { code: string; host: string }) => string {
  const t = useT();
  return ({ code, host }) =>
    t(
      keyExists(`git.test.reasons.${code}`)
        ? `git.test.reasons.${code}`
        : "git.test.reasons.unknown",
      {
        host,
      },
    );
}

// The short form of a failure code, for the Last test tooltip, which has only the code.
export function useShortReason(): (code: string) => string {
  const t = useT();
  return (code) =>
    keyExists(`git.test.short.${code}`)
      ? t(`git.test.short.${code}`)
      : t("git.test.reasons.unknown");
}

interface RepoTestResultProps {
  result: GitTestResult;
  // True when a field the test used has changed since: the result is shown greyed.
  stale?: boolean;
}

// A list of names, mono, text only. Past the first of the list the server sent it says how many
// more there are.
function NameList({
  title,
  names,
  total,
}: {
  title: string;
  names: readonly string[];
  total: number;
}) {
  const t = useT();
  const more = Math.max(total - names.length, 0);
  return (
    <div className="flex min-w-0 flex-col gap-1">
      <h4 className="text-xs font-medium">{title}</h4>
      {names.length === 0 ? (
        <p className="text-xs text-muted-foreground">{t("git.test.none")}</p>
      ) : (
        <ul className="flex flex-col gap-0.5 font-mono text-xs">
          {names.map((n) => (
            <li key={n} className="break-all">
              {n}
            </li>
          ))}
          {more > 0 ? (
            <li className="font-sans text-muted-foreground">
              {t("git.usedBy.more", { count: more })}
            </li>
          ) : null}
        </ul>
      )}
    </div>
  );
}

// RepoTestResult shows the three steps of a repository test, each Passed, Failed, Not needed or
// Not checked as text and a dot (the state is never colour alone), then the summary or the mapped
// failure, then the first 20 branches and tags behind "Show branches and tags". The summary sits in
// an aria-live polite region so it is announced once, when the test settles.
export function RepoTestResult({ result, stale = false }: RepoTestResultProps) {
  const t = useT();
  const reason = useFailureReason();
  const [open, setOpen] = useState(false);
  const steps = [
    { id: "reachable", step: result.steps.reachable },
    { id: "signedIn", step: result.steps.signedIn },
    { id: "refs", step: result.steps.refs },
  ] as const;
  const failure = result.failure;
  const summary = failure
    ? t("git.test.failed", { reason: reason(failure) })
    : result.defaultBranch
      ? t("git.test.summary", {
          branches: result.branchCount,
          tags: result.tagCount,
          name: result.defaultBranch,
        })
      : t("git.test.summaryNoDefault", { branches: result.branchCount, tags: result.tagCount });
  const listed = !failure && (result.branchCount > 0 || result.tagCount > 0);
  return (
    <div className={cn("flex flex-col gap-2", stale && "opacity-60")}>
      <ul className="flex flex-col gap-2">
        {steps.map(({ id, step }) => (
          <li key={id} className="flex flex-wrap items-center justify-between gap-2 text-sm">
            <span>{t(`git.test.steps.${id}`)}</span>
            <StatusDot state={DOT[step.state]} label={t(`git.test.states.${step.state}`)} />
          </li>
        ))}
      </ul>
      <div role="status" aria-live={stale ? "off" : "polite"}>
        <p className={cn("break-words text-sm", failure && "text-status-offline-text")}>
          {summary}
        </p>
      </div>
      {listed ? (
        <Collapsible open={open} onOpenChange={setOpen} className="flex flex-col gap-2">
          <CollapsibleTrigger asChild>
            <Button
              type="button"
              variant="ghost"
              size="sm"
              className="group/refs w-fit max-md:h-11"
            >
              <ChevronRight
                aria-hidden="true"
                className="transition-transform group-aria-expanded/refs:rotate-90"
              />
              {open ? t("git.test.hide") : t("git.test.show")}
            </Button>
          </CollapsibleTrigger>
          <CollapsibleContent className="grid grid-cols-1 gap-4 sm:grid-cols-2">
            <NameList
              title={t("git.test.branches")}
              names={result.branches ?? []}
              total={result.branchCount}
            />
            <NameList
              title={t("git.test.tags")}
              names={result.tags ?? []}
              total={result.tagCount}
            />
          </CollapsibleContent>
        </Collapsible>
      ) : null}
      {stale ? (
        <p className="text-xs text-muted-foreground" aria-live="off">
          {t("git.test.stale")}
        </p>
      ) : null}
    </div>
  );
}
