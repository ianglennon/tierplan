// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { EventDTO } from "@/features/dashboard/events";
import { z } from "@/lib/zod-config";

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

export interface EventFilters {
  // None selected means every type.
  types?: readonly string[];
  // None selected means every action.
  actions?: readonly string[];
  node?: string;
  q?: string;
  // Exactly this name: the chip a row's menu sets.
  name?: string;
}

// applyEventFilters keeps the events that match every filter that is set. The
// search text matches the name, the image and the value of any attribute, ignoring
// case. A row of Bayplan's own has a type no chip names, so it shows only while no
// type is chosen. The input is not changed.
export function applyEventFilters(
  events: readonly EventDTO[],
  { types, actions, node, q, name }: EventFilters,
): EventDTO[] {
  const text = q?.trim().toLowerCase();
  return events.filter((e) => {
    if (types && types.length > 0 && !types.includes(e.type)) return false;
    if (actions && actions.length > 0 && !actions.includes(e.action)) return false;
    if (node !== undefined && e.node !== node) return false;
    if (name !== undefined && e.name !== name) return false;
    if (text) {
      const inName = e.name.toLowerCase().includes(text);
      const inAttributes = Object.values(e.attributes).some((v) => v.toLowerCase().includes(text));
      if (!inName && !inAttributes) return false;
    }
    return true;
  });
}

// The actions after which the resource is gone, so its page would be a 404 and
// the name is plain text (UI-SPEC "Name cell").
const GONE_ACTIONS: ReadonlySet<string> = new Set(["destroy", "remove", "delete"]);

export function isGone(action: string): boolean {
  return GONE_ACTIONS.has(action);
}
