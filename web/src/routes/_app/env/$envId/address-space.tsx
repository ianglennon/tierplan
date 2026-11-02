// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { createFileRoute } from "@tanstack/react-router";

import { AddressSpacePage } from "@/features/address-space/address-space-page";
import {
  MAP_SHOW_VALUES,
  MAP_SORT_VALUES,
  type MapSearch,
} from "@/features/address-space/subnet-map";
import { MAX_QUERY } from "@/features/containers/list-search";
import { nodeSearchSchema } from "@/features/resources/swarm-lists";
import { useDocumentTitle } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { z } from "@/lib/zod-config";

function AddressSpace() {
  const { envId } = Route.useParams();
  const t = useT();
  useDocumentTitle(t("addressSpace.page.title"));
  const search = Route.useSearch();
  const navigate = Route.useNavigate();
  return (
    <AddressSpacePage
      envId={envId}
      search={search}
      onSearchChange={(patch: Partial<MapSearch>) =>
        void navigate({ search: (prev) => ({ ...prev, ...patch }), replace: true })
      }
    />
  );
}

// The page's search: the map's text, chip, node and sort. A value that does not fit is dropped, not
// sent, and "all" is the same as no chip.
const addressSpaceSearchSchema = nodeSearchSchema.extend({
  q: z.string().min(1).max(MAX_QUERY).optional().catch(undefined),
  show: z.enum(MAP_SHOW_VALUES).optional().catch(undefined),
  sort: z.enum(MAP_SORT_VALUES).optional().catch(undefined),
  dir: z.enum(["asc", "desc"]).optional().catch(undefined),
});

export const Route = createFileRoute("/_app/env/$envId/address-space")({
  validateSearch: addressSpaceSearchSchema,
  component: AddressSpace,
});
