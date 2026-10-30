// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { createFileRoute } from "@tanstack/react-router";

import {
  GitPage,
  type RepositoriesSearchPatch,
  repositoriesSearchSchema,
} from "@/features/git/git-page";

function Git() {
  const search = Route.useSearch();
  const navigate = Route.useNavigate();

  // A change to the search or the sheet replaces the history entry, so the back button leaves the
  // page and does not walk through every key.
  function change(patch: RepositoriesSearchPatch) {
    void navigate({
      search: (prev) => {
        const next: Record<string, unknown> = { ...prev, ...patch };
        for (const key of Object.keys(next)) if (next[key] === undefined) delete next[key];
        return next;
      },
      replace: true,
    });
  }

  return <GitPage search={search} onSearchChange={change} />;
}

// The search is the schema in the page: q, scope and repo (the sheet).
export const Route = createFileRoute("/_app/git")({
  validateSearch: repositoriesSearchSchema,
  component: Git,
});
