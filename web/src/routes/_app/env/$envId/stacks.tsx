// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { createFileRoute } from "@tanstack/react-router";

import { type StackListSearchPatch, stackListSearchSchema } from "@/features/stacks/list-search";
import { StackList } from "@/features/stacks/stack-list";
import { useDocumentTitle } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

function Stacks() {
  const { envId } = Route.useParams();
  const t = useT();
  useDocumentTitle(t("stacks.list.title"));
  const search = Route.useSearch();
  const navigate = Route.useNavigate();

  // A change to the search replaces the history entry, so the back button leaves the page and does
  // not walk through every key.
  function change(patch: StackListSearchPatch) {
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
    <StackList
      envId={envId}
      creating={search.new === 1}
      onCreatingChange={(open) => change({ new: open ? 1 : undefined })}
      search={search}
      onSearchChange={change}
    />
  );
}

// The stacks route's search: new=1 opens the Create stack dialog; the search text, the status chips and
// the sort live in the URL too. A value that does not fit is dropped, not sent.
export const Route = createFileRoute("/_app/env/$envId/stacks")({
  validateSearch: stackListSearchSchema,
  component: Stacks,
});
