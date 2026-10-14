// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { createFileRoute } from "@tanstack/react-router";

import { NodeList } from "@/features/nodes/node-list";
import { type NodeSearchPatch, nodeSearchSchema } from "@/features/nodes/node-search";
import { useDocumentTitle } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

function Nodes() {
  const { envId } = Route.useParams();
  const t = useT();
  useDocumentTitle(t("nodes.list.title"));
  const search = Route.useSearch();
  const navigate = Route.useNavigate();

  // A change to the filters, the search or the sort replaces the history entry, so the back
  // button leaves the page and does not walk through every key.
  function change(patch: NodeSearchPatch) {
    void navigate({
      search: (prev) => {
        const next: Record<string, unknown> = { ...prev, ...patch };
        for (const key of Object.keys(next)) if (next[key] === undefined) delete next[key];
        return next;
      },
      replace: true,
    });
  }

  return <NodeList envId={envId} search={search} onSearchChange={change} />;
}

export const Route = createFileRoute("/_app/env/$envId/nodes")({
  validateSearch: nodeSearchSchema,
  component: Nodes,
});
