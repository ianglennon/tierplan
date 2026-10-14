// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { z } from "@/lib/zod-config";

export const placementSearchSchema = z.object({});

export type PlacementSearch = z.output<typeof placementSearchSchema>;
