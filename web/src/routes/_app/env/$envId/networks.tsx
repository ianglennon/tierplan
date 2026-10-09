// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { createFileRoute } from "@tanstack/react-router";

import { NetworkList } from "@/features/networks/network-list";
import { nodeSearchSchema } from "@/features/resources/swarm-lists";
import { useDocumentTitle } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

function Networks() {
  const { envId } = Route.useParams();
  const t = useT();
  useDocumentTitle(t("dashboard.lists.networks.title"));
  const { node } = Route.useSearch();
  const navigate = Route.useNavigate();
  return (
    <NetworkList
      envId={envId}
      node={node}
      onNodeChange={(n) => void navigate({ search: { node: n }, replace: true })}
    />
  );
}

export const Route = createFileRoute("/_app/env/$envId/networks")({
  validateSearch: nodeSearchSchema,
  component: Networks,
});
