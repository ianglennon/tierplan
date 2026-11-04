// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { createFileRoute } from "@tanstack/react-router";
import { type SearchPageSearch, searchPageSchema } from "@/features/search/search-params";
import { SearchResultsPage } from "@/features/search/search-results-page";

function SearchRoute() {
  const search = Route.useSearch();
  const navigate = Route.useNavigate();

  // A change replaces the history entry, so the Back button leaves the page and does not walk
  // through every key; a key set to undefined leaves the address.
  const change = (patch: Partial<SearchPageSearch>) =>
    void navigate({
      search: (prev) => {
        const next: Record<string, unknown> = { ...prev, ...patch };
        for (const key of Object.keys(next)) if (next[key] === undefined) delete next[key];
        return next;
      },
      replace: true,
    });

  return <SearchResultsPage search={search} onSearchChange={change} />;
}

// The query, the type chip and the environments are the address (D-18). A value the page does not
// know is dropped. The query is shown as text and never put in a path.
export const Route = createFileRoute("/_app/search")({
  validateSearch: searchPageSchema,
  component: SearchRoute,
});
