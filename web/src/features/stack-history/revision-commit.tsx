// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import type { StackRevision } from "@/api/stack-history";
import type { StackDetail } from "@/api/stacks";
import { ShaLink } from "@/features/stack-git/sha-link";
import { useT } from "@/i18n/use-t";

// The repository a git stack follows now, as its summary names it. A revision made from another repository gets no
// commit link, since the address here is not where its commit lives.
export interface HistoryRepo {
  id: string;
  host: string;
  path: string;
}

// historyRepoOf is the repository a stack follows, or undefined for a stack that is not from git.
export function historyRepoOf(stack: StackDetail | undefined): HistoryRepo | undefined {
  const git = stack?.git;
  return git ? { id: git.repoId, host: git.repoHost, path: git.repoPath } : undefined;
}

// RevisionCommit is the commit a revision was made from, in mono: a link on a known host, plain text elsewhere (UI-SPEC
// "History additions"). A revision from before the stack used git has none and reads as a dash.
export function RevisionCommit({
  item,
  repo,
}: {
  item: StackRevision;
  repo: HistoryRepo | undefined;
}) {
  const t = useT();
  if (!item.commit) return <span aria-hidden="true">{t("stackHistory.missing")}</span>;
  const same = repo !== undefined && item.source?.repoId === repo.id;
  return (
    <ShaLink sha={item.commit} repoHost={same ? repo.host : ""} repoPath={same ? repo.path : ""} />
  );
}
