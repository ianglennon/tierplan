// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { MAX_QUERY } from "@/features/containers/list-search";
import { z } from "@/lib/zod-config";

// The layouts of the Placement page. A missing view means the first, By node. The grid is
// added to the page by a later plan; the URL already accepts it.
export const PLACEMENT_VIEWS = ["nodes", "grid"] as const;
export type PlacementViewName = (typeof PLACEMENT_VIEWS)[number];

// The Placement route's search (UI-SPEC "Routes"): the layout, the stack (a stack name, or none
// for services in no stack), the service search and the live-stats switch. Every value that
// does not fit is dropped, not failed on.
export const placementSearchSchema = z.object({
  view: z.enum(PLACEMENT_VIEWS).optional().catch(undefined),
  stack: z.string().min(1).max(MAX_QUERY).optional().catch(undefined),
  q: z.string().min(1).max(MAX_QUERY).optional().catch(undefined),
  stats: z.literal(1).optional().catch(undefined),
});

export type PlacementSearch = z.output<typeof placementSearchSchema>;

// A change to the search: a key set to undefined is taken out of the URL.
export type PlacementSearchPatch = {
  [K in keyof PlacementSearch]?: PlacementSearch[K] | undefined;
};
