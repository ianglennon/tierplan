// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { CredentialTestResult, RegistryType, TestStep } from "@/api/registries";
import { StatusDot } from "@/components/status-dot";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

interface TestResultProps {
  result: Pick<CredentialTestResult, "reachable" | "signedIn" | "canPull">;
  // The registry host the test ran against, and the repository it tried to read ("" for none).
  host: string;
  repository: string;
  type: RegistryType;
  // True when a field the test used has changed since: the result is shown greyed.
  stale?: boolean;
}

const DOT = { passed: "online", failed: "offline", not_checked: "unknown" } as const;

// The reason a step failed, or why it was not checked, in the words of UI-SPEC "Shared
// refusals". The server sends a code and never any text from the registry, so nothing the
// registry said can reach the page.
function useReason(host: string, repository: string) {
  const t = useT();
  return (step: TestStep): string | null => {
    if (step.code === "") return null;
    const known = [
      "registry_unreachable",
      "registry_tls",
      "registry_http",
      "sign_in_refused",
      "no_login_asked",
      "no_pull_access",
      "repository_not_found",
      "rate_limited",
    ];
    const key = known.includes(step.code) ? step.code : "check_failed";
    return t(`registries.test.reasons.${key}`, { host, repo: repository });
  };
}

// TestResult shows the three steps of a credential test, each Passed, Failed or Not
// checked with its reason, as text and a dot (the state is never colour alone). The region
// is a status, announced once when the test finishes.
export function TestResult({ result, host, repository, type, stale = false }: TestResultProps) {
  const t = useT();
  const reason = useReason(host, repository);
  const steps: { id: string; label: string; step: TestStep }[] = [
    { id: "reachable", label: t("registries.test.steps.reachable"), step: result.reachable },
    { id: "signedIn", label: t("registries.test.steps.signedIn"), step: result.signedIn },
    {
      id: "canPull",
      label: repository
        ? t("registries.test.steps.canPull", { repo: repository })
        : t("registries.test.steps.canPullNone"),
      step: result.canPull,
    },
  ];
  const failed = steps.find((s) => s.step.state === "failed");
  const summary = failed
    ? t("registries.test.failed", { reason: reason(failed.step) ?? "" })
    : t("registries.test.passed");
  // GitHub may refuse a token until it names a repository the token covers (RESEARCH A2).
  const ghcrHint =
    type === "ghcr" &&
    repository === "" &&
    result.signedIn.state === "failed" &&
    result.signedIn.code === "sign_in_refused";
  return (
    <div
      role="status"
      aria-live={stale ? "off" : "polite"}
      className={cn("flex flex-col gap-2", stale && "opacity-60")}
    >
      <p className="sr-only">{summary}</p>
      <ul className="flex flex-col gap-2">
        {steps.map(({ id, label, step }) => {
          const why = reason(step);
          return (
            <li key={id} className="flex flex-col gap-0.5">
              <div className="flex flex-wrap items-center justify-between gap-2 text-sm">
                <span>{label}</span>
                <StatusDot
                  state={DOT[step.state]}
                  label={t(`registries.test.states.${step.state}`)}
                />
              </div>
              {why ? <p className="text-xs text-muted-foreground">{why}</p> : null}
            </li>
          );
        })}
      </ul>
      {ghcrHint ? (
        <p className="text-xs text-muted-foreground">{t("registries.test.ghcrHint")}</p>
      ) : null}
      {stale ? (
        <p className="text-xs text-muted-foreground" aria-live="off">
          {t("registries.test.stale")}
        </p>
      ) : null}
    </div>
  );
}
