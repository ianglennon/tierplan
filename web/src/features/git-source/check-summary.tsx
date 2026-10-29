// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { CircleX, TriangleAlert } from "lucide-react";
import { useCallback, useEffect, useId, useRef, useState } from "react";

import {
  type GitRequestError,
  type GitStackCheck,
  type GitStackCheckRequest,
  useCheckGitStackSource,
} from "@/api/git";
import type { StackProblem } from "@/api/stacks";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { useBrowseReason } from "@/features/git-source/browse-reason";
import { useT } from "@/i18n/use-t";

// How long after the last change to the choices the files are checked (UI-SPEC "Lint and inline
// errors"): a person typing a variable value costs one request, not one per key.
export const gitCheckDebounceMs = 400;

// The services listed by name before the rest are counted.
const NAMED_SERVICES = 5;

export interface GitCheckState {
  // The last answer of the server. It stays while the next check runs, so a table that is being
  // typed in does not lose its rows, and after a failed check.
  check?: GitStackCheck;
  // A check is waiting for the person to stop changing things, or is on its way.
  checking: boolean;
  // Why the last check got no answer.
  error?: GitRequestError;
  // Check again now.
  retry: () => void;
}

// useGitSourceCheck checks what a repository folder and its compose files would give (GIT-08),
// 400 ms after the last change to the request. It saves nothing and is never retried by itself:
// the next change sends the next check, and a failed one offers Retry. An answer to a request that
// has since changed is dropped. A null request is nothing to check yet and clears the result.
export function useGitSourceCheck(
  envId: string,
  request: GitStackCheckRequest | null,
): GitCheckState {
  const { mutateAsync } = useCheckGitStackSource(envId);
  const send = useRef(mutateAsync);
  send.current = mutateAsync;
  const latest = useRef(0);
  const current = useRef(request);
  current.current = request;
  const [check, setCheck] = useState<GitStackCheck | undefined>(undefined);
  const [error, setError] = useState<GitRequestError | undefined>(undefined);
  const [checking, setChecking] = useState(false);
  const key = request ? JSON.stringify(request) : "";

  const run = useCallback(() => {
    const body = current.current;
    if (!body) return;
    const mine = ++latest.current;
    setChecking(true);
    send.current(body).then(
      (answer) => {
        if (mine !== latest.current) return;
        setCheck(answer);
        setError(undefined);
        setChecking(false);
      },
      (err: GitRequestError) => {
        if (mine !== latest.current) return;
        setError(err);
        setChecking(false);
      },
    );
  }, []);

  useEffect(() => {
    if (key === "") {
      latest.current++;
      setCheck(undefined);
      setError(undefined);
      setChecking(false);
      return;
    }
    setChecking(true);
    const timer = setTimeout(run, gitCheckDebounceMs);
    return () => clearTimeout(timer);
  }, [key, run]);

  // A page that closes sends nothing more and takes no answer.
  useEffect(
    () => () => {
      latest.current++;
    },
    [],
  );

  return { check, checking, error, retry: run };
}

interface CheckSummaryProps {
  state: GitCheckState;
  // The repository's name, for the wording of a failed check.
  repoName: string;
  // Offered on a missing file: opens Add overlay file with the path filled in.
  onAddOverlay?: (path: string) => void;
}

function ServicesLine({ services }: { services: readonly string[] }) {
  const t = useT();
  if (services.length === 0) return <p className="font-medium">{t("gitSource.check.none")}</p>;
  const named = services.slice(0, NAMED_SERVICES);
  const more = services.length - named.length;
  return (
    <p className="min-w-0 break-words">
      <span className="font-medium">{t("gitSource.check.found", { count: services.length })}</span>{" "}
      {named.map((name, i) => (
        <span key={name}>
          <code className="font-mono">{name}</code>
          {i < named.length - 1 ? ", " : ""}
        </span>
      ))}
      {more > 0 ? ` ${t("gitSource.check.more", { count: more })}` : ""}.
    </p>
  );
}

function ProblemRow({
  problem,
  onAddOverlay,
}: {
  problem: StackProblem;
  onAddOverlay?: () => void;
}) {
  const t = useT();
  const where = problem.file
    ? problem.line > 0
      ? `${problem.file}:${problem.line}`
      : problem.file
    : "";
  return (
    <li className="flex flex-wrap items-start gap-x-2 gap-y-1">
      <span className="sr-only">{t(`gitSource.check.severity.${problem.severity}`)}: </span>
      <span className="min-w-0 flex-1 break-words">{problem.message}</span>
      {where ? (
        <code className="shrink-0 font-mono text-xs text-muted-foreground">{where}</code>
      ) : null}
      {onAddOverlay ? (
        <Button
          type="button"
          variant="outline"
          size="sm"
          className="max-md:h-11"
          onClick={onAddOverlay}
        >
          {t("gitSource.check.addOverlay")}
        </Button>
      ) : null}
    </li>
  );
}

// CheckSummary is what the chosen folder deploys and everything wrong with it (UI-SPEC "Check
// summary"): the services found, the blocking problems in an alert that says to fix them in the
// repository, and the warnings in a warning alert, each with its file and line. The result is
// announced politely, once, when it settles; while the next check runs the last result stays, marked
// busy. It reads and saves nothing, and every message and path is drawn as text.
export function CheckSummary({ state, repoName, onAddOverlay }: CheckSummaryProps) {
  const t = useT();
  const reason = useBrowseReason();
  const { check, checking, error, retry } = state;
  const problems = check?.problems ?? [];
  const blocking = problems.filter((p) => p.severity === "error");
  const warnings = problems.filter((p) => p.severity !== "error");
  const missing = check?.missing ?? [];
  const overlayFor = (p: StackProblem) => {
    if (!onAddOverlay || p.code !== "repo_file_missing") return undefined;
    const path = missing.find((m) => p.message.startsWith(m));
    return path === undefined ? undefined : () => onAddOverlay(path);
  };
  const first = checking && !check;
  const titleId = useId();

  return (
    <div aria-busy={checking || undefined} className="flex min-w-0 flex-col gap-3">
      {first ? (
        <div className="flex flex-col gap-2" data-testid="git-check-loading">
          <p className="text-sm text-muted-foreground">{t("gitSource.check.checking")}</p>
          <Skeleton className="h-5 w-2/3" aria-hidden="true" />
        </div>
      ) : null}

      {error && !checking ? (
        <Alert variant="destructive" role="alert">
          <CircleX aria-hidden="true" />
          <AlertDescription className="flex flex-wrap items-center gap-3">
            <span className="min-w-0 break-words">
              {t("gitSource.check.failed", { reason: reason(error, repoName) })}
            </span>
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="max-md:h-11"
              onClick={retry}
            >
              {t("gitSource.check.retry")}
            </Button>
          </AlertDescription>
        </Alert>
      ) : null}

      {/* The one announcement of a settled result: the region is filled in when a check ends. */}
      <div role="status" aria-live="polite" className="flex min-w-0 flex-col gap-3 text-sm">
        {check && !error ? (
          <>
            <ServicesLine services={check.services ?? []} />
            {blocking.length > 0 ? (
              <Alert variant="destructive" role="group" aria-labelledby={titleId}>
                <CircleX aria-hidden="true" />
                <AlertTitle id={titleId}>{t("gitSource.check.blockingTitle")}</AlertTitle>
                <AlertDescription>
                  <ul className="flex flex-col gap-2">
                    {blocking.map((p) => (
                      <ProblemRow
                        key={`${p.file}:${p.line}:${p.col}:${p.code}:${p.message}`}
                        problem={p}
                        onAddOverlay={overlayFor(p)}
                      />
                    ))}
                  </ul>
                </AlertDescription>
              </Alert>
            ) : null}
            {warnings.length > 0 ? (
              <Alert variant="warning" role="group">
                <TriangleAlert aria-hidden="true" />
                <AlertDescription>
                  <ul className="flex flex-col gap-2">
                    {warnings.map((p) => (
                      <ProblemRow
                        key={`${p.file}:${p.line}:${p.col}:${p.code}:${p.message}`}
                        problem={p}
                      />
                    ))}
                  </ul>
                </AlertDescription>
              </Alert>
            ) : null}
          </>
        ) : null}
      </div>
    </div>
  );
}
