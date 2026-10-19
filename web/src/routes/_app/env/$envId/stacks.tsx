// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { createFileRoute } from "@tanstack/react-router";

import { StackList } from "@/features/stacks/stack-list";
import { useDocumentTitle } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { z } from "@/lib/zod-config";

function Stacks() {
  const { envId } = Route.useParams();
  const t = useT();
  useDocumentTitle(t("stacks.list.title"));
  const { new: creating } = Route.useSearch();
  const navigate = Route.useNavigate();
  return (
    <StackList
      envId={envId}
      creating={creating === 1}
      onCreatingChange={(open) =>
        void navigate({ search: (prev) => ({ ...prev, new: open ? 1 : undefined }), replace: true })
      }
    />
  );
}

// The stacks route's search: new=1 opens the Create stack dialog. A value that does not fit is
// dropped, not sent.
const stackSearchSchema = z.object({
  new: z.literal(1).optional().catch(undefined),
});

export const Route = createFileRoute("/_app/env/$envId/stacks")({
  validateSearch: stackSearchSchema,
  component: Stacks,
});
