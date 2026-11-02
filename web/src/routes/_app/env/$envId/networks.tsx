// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { createFileRoute } from "@tanstack/react-router";
import { MAX_QUERY } from "@/features/containers/list-search";
import { NetworkList } from "@/features/networks/network-list";
import { nodeSearchSchema } from "@/features/resources/swarm-lists";
import { useDocumentTitle } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { z } from "@/lib/zod-config";

function Networks() {
  const { envId } = Route.useParams();
  const t = useT();
  useDocumentTitle(t("dashboard.lists.networks.title"));
  const { node, q } = Route.useSearch();
  const navigate = Route.useNavigate();
  return (
    <NetworkList
      envId={envId}
      node={node}
      q={q}
      onNodeChange={(n) =>
        void navigate({ search: (prev) => ({ ...prev, node: n }), replace: true })
      }
      onSearchChange={(next) =>
        void navigate({ search: (prev) => ({ ...prev, q: next }), replace: true })
      }
    />
  );
}

// The networks route's search: the node and the search text. A search that does not
// fit is dropped, not sent.
const networkSearchSchema = nodeSearchSchema.extend({
  q: z.string().min(1).max(MAX_QUERY).optional().catch(undefined),
});

export const Route = createFileRoute("/_app/env/$envId/networks")({
  validateSearch: networkSearchSchema,
  component: Networks,
});
