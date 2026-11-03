// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { createFileRoute } from "@tanstack/react-router";

import { JanitorPage } from "@/features/janitor/janitor-page";
import { useDocumentTitle } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { z } from "@/lib/zod-config";

function Janitor() {
  const { envId } = Route.useParams();
  const { limit } = Route.useSearch();
  const navigate = Route.useNavigate();
  const t = useT();
  useDocumentTitle(t("janitor.page.title"));
  return (
    <JanitorPage
      envId={envId}
      limit={limit}
      onLimitChange={(next) =>
        void navigate({
          search: (prev) => ({ ...prev, limit: next === 25 ? undefined : next }),
          replace: true,
        })
      }
    />
  );
}

// The size of the run history shown (25 by default, up to 100). A value that does not fit is
// dropped, so a stale or hand-edited link opens the default.
const janitorSearchSchema = z.object({
  limit: z.coerce.number().int().min(25).max(100).optional().catch(undefined),
});

export const Route = createFileRoute("/_app/env/$envId/janitor")({
  validateSearch: janitorSearchSchema,
  component: Janitor,
});
