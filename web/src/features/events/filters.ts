// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { z } from "zod";

// The types the Events page filters by. A row of Bayplan's own has no chip and
// shows under "none selected" only.
export const DOCKER_TYPES = [
  "container",
  "image",
  "network",
  "volume",
  "service",
  "node",
  "secret",
  "config",
] as const;
export type DockerType = (typeof DOCKER_TYPES)[number];

// The types only a swarm manager reports; their chips show on a swarm alone.
export const SWARM_TYPES: readonly DockerType[] = ["service", "node", "secret", "config"];

// "Show events from": how far back the first read goes. The server keeps 24 hours
// at most (04-26), so nothing longer is offered.
export const RANGES = ["5m", "1h", "24h"] as const;
export type Range = (typeof RANGES)[number];
export const DEFAULT_RANGE: Range = "1h";

const RANGE_MS: Record<Range, number> = {
  "5m": 5 * 60_000,
  "1h": 60 * 60_000,
  "24h": 24 * 60 * 60_000,
};

// rangeStart is the RFC 3339 time a range reaches back to from `now`.
export function rangeStart(range: Range, now: number): string {
  return new Date(now - RANGE_MS[range]).toISOString();
}

// How long the free text, a node and a name may be. A value that does not fit is
// dropped from the URL rather than used (T-04-102).
const MAX_TEXT = 128;
const MAX_NAME = 256;
const MAX_ACTIONS = 32;

// The Events route's search (D-48): the range, the type chips, the actions, the
// node, the search text and the name chip. Every value that does not fit is
// dropped, never sent or matched, and the default range stays out of the URL.
export const eventsSearchSchema = z.object({
  since: z.enum(RANGES).optional().catch(undefined),
  type: z.array(z.enum(DOCKER_TYPES)).max(DOCKER_TYPES.length).optional().catch(undefined),
  action: z.array(z.string().min(1).max(64)).max(MAX_ACTIONS).optional().catch(undefined),
  node: z.string().min(1).max(MAX_TEXT).optional().catch(undefined),
  q: z.string().min(1).max(MAX_TEXT).optional().catch(undefined),
  name: z.string().min(1).max(MAX_NAME).optional().catch(undefined),
});

export type EventsSearch = z.output<typeof eventsSearchSchema>;

// A change to the search: a key set to undefined leaves the URL.
export type EventsSearchPatch = { [K in keyof EventsSearch]?: EventsSearch[K] | undefined };
