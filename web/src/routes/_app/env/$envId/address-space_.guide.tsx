// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { createFileRoute } from "@tanstack/react-router";

import { GUIDE_PARTS, PoolGuidePage } from "@/features/pool-guide/pool-guide-page";
import { useDocumentTitle } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { z } from "@/lib/zod-config";

function PoolGuide() {
  const { envId } = Route.useParams();
  const { part } = Route.useSearch();
  const t = useT();
  useDocumentTitle(t("poolGuide.title"));
  return <PoolGuidePage envId={envId} part={part} />;
}

// The guidance part is the only search: a value that does not fit is dropped, so a stale or
// hand-edited link opens the default part. What is typed on the page never goes in the address.
const guideSearchSchema = z.object({
  part: z.enum(GUIDE_PARTS).optional().catch(undefined),
});

export const Route = createFileRoute("/_app/env/$envId/address-space_/guide")({
  validateSearch: guideSearchSchema,
  component: PoolGuide,
});
