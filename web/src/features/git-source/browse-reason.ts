// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { GitRequestError } from "@/api/git";
import { useFailureReason } from "@/features/git/repo-test-result";
import { keyExists, useT } from "@/i18n/use-t";

// useBrowseReason words why a repository's refs or folders could not be read, in the copy of
// UI-SPEC "Shared refusals and failures". The server answers 422 with the git code as the problem
// type and the host as a field; it never sends text from the git host, so none can reach the page.
// A code this version does not know gets the general line.
export function useBrowseReason(): (err: GitRequestError, repo: string) => string {
  const t = useT();
  const failure = useFailureReason();
  return (err, repo) => {
    if (err.status === 0) return t("gitSource.reasons.offline");
    const code = err.type ?? "";
    const host = err.fields.find((f) => f.location === "host")?.message ?? "";
    if (keyExists(`gitSource.reasons.${code}`))
      return t(`gitSource.reasons.${code}`, { repo, host });
    if (keyExists(`git.test.reasons.${code}`)) return failure({ code, host });
    if (err.status === 403) return t("gitSource.reasons.permission");
    return t("gitSource.reasons.unknown");
  };
}
