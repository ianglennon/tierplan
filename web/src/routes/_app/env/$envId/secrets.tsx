// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { createFileRoute } from "@tanstack/react-router";

import { MAX_QUERY } from "@/features/containers/list-search";
import { SecretList } from "@/features/secrets/secret-list";
import { useDocumentTitle } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { z } from "@/lib/zod-config";

function Secrets() {
  const { envId } = Route.useParams();
  const t = useT();
  useDocumentTitle(t("secrets.list.title"));
  const { q, new: creating } = Route.useSearch();
  const navigate = Route.useNavigate();
  return (
    <SecretList
      envId={envId}
      q={q}
      onSearchChange={(next) =>
        void navigate({ search: (prev) => ({ ...prev, q: next }), replace: true })
      }
      creating={creating === 1}
      onCreatingChange={(open) =>
        void navigate({ search: (prev) => ({ ...prev, new: open ? 1 : undefined }), replace: true })
      }
    />
  );
}

// The secrets route's search: the search text, and new=1, which opens the create sheet. A search
// that does not fit is dropped, not sent.
const secretSearchSchema = z.object({
  q: z.string().min(1).max(MAX_QUERY).optional().catch(undefined),
  new: z.literal(1).optional().catch(undefined),
});

export const Route = createFileRoute("/_app/env/$envId/secrets")({
  validateSearch: secretSearchSchema,
  component: Secrets,
});
