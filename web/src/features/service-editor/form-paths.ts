// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { parsePath } from "@/features/service-editor/spec-patch";

// FORM_OWNED_PATHS are the paths of a swarm.ServiceSpec a control of the Form tab
// reads and writes (D-01). Everything else in a loaded spec is carried through
// untouched, and the caution bar names it (D-02). A control that is added to the form adds its
// path here, so the caution bar and the form can't disagree.
export const FORM_OWNED_PATHS: readonly string[] = [
  // Basics
  "Name",
  "Labels",
  "Mode.Replicated.Replicas",
  "Mode.Global",
  "TaskTemplate.ContainerSpec.Image",
  // Environment
  "TaskTemplate.ContainerSpec.Env",
  // Ports
  "EndpointSpec.Ports",
  // Resources
  "TaskTemplate.Resources.Limits.NanoCPUs",
  "TaskTemplate.Resources.Limits.MemoryBytes",
  "TaskTemplate.Resources.Reservations.NanoCPUs",
  "TaskTemplate.Resources.Reservations.MemoryBytes",
  // Secrets, configs and networks: the whole lists, because a row is a reference. A row changes the
  // object, its file, UID, GID and mode (or a network's aliases) and carries the rest of the
  // reference as it is.
  "TaskTemplate.ContainerSpec.Secrets",
  "TaskTemplate.ContainerSpec.Configs",
  "TaskTemplate.Networks",
  // Mounts: the whole list, because a row is a mount. The card changes the type, source,
  // destination and read-only flag of a mount and carries the rest of it (volume options) as it is.
  "TaskTemplate.ContainerSpec.Mounts",
  // Placement. Platforms are not here: Coaming sets them when it pins an image.
  "TaskTemplate.Placement.Constraints",
  "TaskTemplate.Placement.Preferences",
  "TaskTemplate.Placement.MaxReplicas",
  // Restart policy
  "TaskTemplate.RestartPolicy.Condition",
  "TaskTemplate.RestartPolicy.Delay",
  "TaskTemplate.RestartPolicy.MaxAttempts",
  "TaskTemplate.RestartPolicy.Window",
  // Update policy, with the auto-rollback box (UpdateConfig.FailureAction). RollbackConfig is
  // not here.
  "UpdateConfig.Parallelism",
  "UpdateConfig.Delay",
  "UpdateConfig.FailureAction",
  "UpdateConfig.Monitor",
  "UpdateConfig.MaxFailureRatio",
  "UpdateConfig.Order",
];

type Segment = string | number;

const OWNED_SEGMENTS: readonly (readonly Segment[])[] = FORM_OWNED_PATHS.map(parsePath);

// owns is true when an owned path is the path or lies above it: everything under
// an owned path (a label, an env entry, a port) belongs to the same control.
function owns(segs: readonly Segment[]): boolean {
  return OWNED_SEGMENTS.some((o) => o.length <= segs.length && o.every((s, i) => segs[i] === s));
}

// holdsOwned is true when an owned path lies strictly below the path: the walk has
// to look inside such a value, because only some of it is the form's.
function holdsOwned(segs: readonly Segment[]): boolean {
  return OWNED_SEGMENTS.some((o) => o.length > segs.length && segs.every((s, i) => o[i] === s));
}

// isFormOwned is true when a form control owns the path or a path above it.
export function isFormOwned(path: string): boolean {
  try {
    return owns(parsePath(path));
  } catch {
    return false;
  }
}

const isRec = (v: unknown): v is Record<string, unknown> =>
  typeof v === "object" && v !== null && !Array.isArray(v);

// populated is false for a null, an empty string, an empty list and an object with
// nothing populated in it. A false or a 0 is a value the spec states, so it counts.
function populated(v: unknown): boolean {
  if (v === null || v === undefined || v === "") return false;
  if (Array.isArray(v)) return v.length > 0;
  if (isRec(v)) return Object.values(v).some(populated);
  return true;
}

function walk(node: unknown, segs: string[], out: string[]): void {
  if (!populated(node)) return;
  if (owns(segs)) return;
  if (isRec(node) && holdsOwned(segs)) {
    for (const [k, v] of Object.entries(node)) walk(v, [...segs, k], out);
    return;
  }
  out.push(segs.join("."));
}

// unshownPaths lists the populated settings of a spec that no form control owns, in
// document order, each named once at the shallowest path that holds none of the
// form's: "TaskTemplate.ContainerSpec.Healthcheck", not the fields inside it. These
// are the settings the caution bar names; saving from the form keeps them as they
// are because the form only ever patches the paths it owns.
export function unshownPaths(spec: unknown): string[] {
  if (!isRec(spec)) return [];
  const out: string[] = [];
  walk(spec, [], out);
  return out;
}
