// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { createFileRoute } from "@tanstack/react-router";

import { credentialsSearchSchema, GitPage, type GitSearchPatch } from "@/features/git/git-page";

// The trailing underscore on the file name keeps the page outside the Repositories route, so it
// replaces that page and does not render under it.
function GitCredentials() {
  const search = Route.useSearch();
  const navigate = Route.useNavigate();

  // A change to the search or the sheet replaces the history entry, so the back button leaves the
  // page and does not walk through every key.
  function change(patch: GitSearchPatch) {
    void navigate({
      search: (prev) => {
        const next: Record<string, unknown> = { ...prev, ...patch };
        for (const key of Object.keys(next)) if (next[key] === undefined) delete next[key];
        return next;
      },
      replace: true,
    });
  }

  return <GitPage tab="credentials" search={search} onSearchChange={change} />;
}

// The search is the schema in the page: q, scope and credential (the sheet).
export const Route = createFileRoute("/_app/git_/credentials")({
  validateSearch: credentialsSearchSchema,
  component: GitCredentials,
});
