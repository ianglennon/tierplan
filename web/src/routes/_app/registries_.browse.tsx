// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { createFileRoute } from "@tanstack/react-router";

import {
  BrowsePage,
  type BrowseSearch,
  type BrowseSearchPatch,
  browseSearchSchema,
} from "@/features/tags/browse-page";

// The trailing underscore on the file name keeps the page outside the Registries route, so it
// replaces that page and does not render under it.
function Browse() {
  const search = Route.useSearch();
  const navigate = Route.useNavigate();

  // A change to the filter replaces the history entry, so the back button leaves the page and
  // does not walk through every key.
  function change(patch: BrowseSearchPatch) {
    void navigate({
      search: (prev) => {
        const merged = { ...prev, ...patch };
        const next: BrowseSearch = { ref: merged.ref };
        if (merged.q !== undefined) next.q = merged.q;
        if (merged.hide !== undefined) next.hide = merged.hide;
        if (merged.envId !== undefined) next.envId = merged.envId;
        return next;
      },
      replace: true,
    });
  }

  return <BrowsePage search={search} onSearchChange={change} />;
}

export const Route = createFileRoute("/_app/registries_/browse")({
  validateSearch: browseSearchSchema,
  component: Browse,
});
