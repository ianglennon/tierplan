// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { createFileRoute } from "@tanstack/react-router";

import { ConfigList } from "@/features/configs/config-list";
import { MAX_QUERY } from "@/features/containers/list-search";
import { useDocumentTitle } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { z } from "@/lib/zod-config";

function Configs() {
  const { envId } = Route.useParams();
  const t = useT();
  useDocumentTitle(t("configs.list.title"));
  const { q } = Route.useSearch();
  const navigate = Route.useNavigate();
  return (
    <ConfigList
      envId={envId}
      q={q}
      onSearchChange={(next) =>
        void navigate({ search: (prev) => ({ ...prev, q: next }), replace: true })
      }
    />
  );
}

// The configs route's search: the search text. A search that does not fit is dropped, not sent.
const configSearchSchema = z.object({
  q: z.string().min(1).max(MAX_QUERY).optional().catch(undefined),
});

export const Route = createFileRoute("/_app/env/$envId/configs")({
  validateSearch: configSearchSchema,
  component: Configs,
});
