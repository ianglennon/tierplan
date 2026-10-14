// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { createFileRoute } from "@tanstack/react-router";

import { nodeQuery } from "@/api/nodes";
import { type KeyValueRow, KeyValueTable } from "@/components/key-value-table";
import { Skeleton } from "@/components/ui/skeleton";
import { useT } from "@/i18n/use-t";

// The Labels tab of a node: the labels services use in their placement constraints, by key. They
// are read-only here; the editor comes with a later plan. The page frame owns the gone state and
// the load error, so this draws the labels once the node is there.
function NodeLabelsRoute() {
  const t = useT();
  const { envId, nodeId } = Route.useParams();
  const { data } = useQuery(nodeQuery(envId, nodeId));
  if (!data) return <Skeleton className="h-40 w-full" />;
  const rows: KeyValueRow[] = Object.entries(data.labels)
    .sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))
    .map(([key, value]) => ({ key, value }));
  return (
    <section aria-labelledby="node-labels-heading" className="flex flex-col gap-3">
      <h3 id="node-labels-heading" className="text-base font-semibold">
        {t("nodes.labels.title")}
      </h3>
      <p className="text-sm text-muted-foreground">{t("nodes.labels.helper")}</p>
      <KeyValueTable label={t("nodes.labels.title")} empty={t("nodes.labels.empty")} rows={rows} />
    </section>
  );
}

export const Route = createFileRoute("/_app/env/$envId/nodes_/$nodeId/labels")({
  component: NodeLabelsRoute,
});
