// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { createFileRoute } from "@tanstack/react-router";

import { GlobalStackList } from "@/features/stacks/global-stack-list";
import {
  type GlobalStackSearchPatch,
  globalStackSearchSchema,
} from "@/features/stacks/list-search";
import { useDocumentTitle } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

function AllStacks() {
  const t = useT();
  useDocumentTitle(t("stackLists.documentTitle"));
  const search = Route.useSearch();
  const navigate = Route.useNavigate();

  // A change to the search replaces the history entry, so the back button leaves the page and does
  // not walk through every key.
  function change(patch: GlobalStackSearchPatch) {
    void navigate({
      search: (prev) => {
        const next: Record<string, unknown> = { ...prev, ...patch };
        for (const key of Object.keys(next)) if (next[key] === undefined) delete next[key];
        return next;
      },
      replace: true,
    });
  }

  return (
    <GlobalStackList
      search={search}
      onSearchChange={change}
      creating={search.new === 1}
      onCreatingChange={(open) => change({ new: open ? 1 : undefined })}
    />
  );
}

// The search is the schema in the list: the environments chosen, the grouping, the search text, the
// status chips and the sort live in the URL (D-21).
export const Route = createFileRoute("/_app/stacks")({
  validateSearch: globalStackSearchSchema,
  component: AllStacks,
});
