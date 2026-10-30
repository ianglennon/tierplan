// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { getPath, removePath, setPath } from "@/features/service-editor/spec-patch";
import type { SpecValue } from "@/features/spec-editor/spec-format";

// The auto-rollback choice (D-12 amended): "Roll back automatically if an update fails" is the
// service's own UpdateConfig.FailureAction, rollback when ticked and pause (Docker's default)
// when not. The Update policy card and the Review step both show it, and both change it here.

const UPDATE_CONFIG = "UpdateConfig";
const FAILURE_ACTION = "UpdateConfig.FailureAction";

// What `docker service create` gives a service that states no update policy. A service with no
// UpdateConfig updates one task at a time, stopping each before starting its replacement; an
// UpdateConfig that says Parallelism 0 updates all of them at once. So the first time the form
// creates an UpdateConfig it has to say what the absent one meant, or ticking the box (or typing
// a delay) would quietly change how fast a rollout goes (RESEARCH Pitfall 5).
export const AUTO_ROLLBACK_DEFAULTS = {
  Parallelism: 1,
  Delay: 0,
  Monitor: 5_000_000_000,
  MaxFailureRatio: 0,
  Order: "stop-first",
} as const;

// The UpdateConfig that means the same as none at all, in the order Docker writes its fields.
function defaultConfig(action: string): Record<string, unknown> {
  const d = AUTO_ROLLBACK_DEFAULTS;
  return {
    Parallelism: d.Parallelism,
    Delay: d.Delay,
    FailureAction: action,
    Monitor: d.Monitor,
    MaxFailureRatio: d.MaxFailureRatio,
    Order: d.Order,
  };
}

const absent = (spec: unknown): boolean => {
  const v = getPath(spec, UPDATE_CONFIG);
  return v === undefined || v === null;
};

// isDefaultConfig is true for an UpdateConfig that says exactly what the CLI defaults say.
function isDefaultConfig(v: unknown): boolean {
  if (typeof v !== "object" || v === null || Array.isArray(v)) return false;
  const want = defaultConfig("pause");
  const have = v as Record<string, unknown>;
  const keys = Object.keys(have);
  return keys.length === Object.keys(want).length && keys.every((k) => have[k] === want[k]);
}

// seedUpdateConfig returns the spec with an UpdateConfig of the CLI defaults when it has none,
// and the spec itself when it has one.
export function seedUpdateConfig(spec: SpecValue): SpecValue {
  return absent(spec) ? setPath(spec, UPDATE_CONFIG, defaultConfig("pause")) : spec;
}

// setAutoRollback returns the spec with the choice applied and nothing else changed. Ticked on a
// service with no UpdateConfig it creates one with the defaults above; unticked on such a
// service it leaves it absent. On an UpdateConfig that exists, only FailureAction changes, and
// only when it has to: pause is already what unticked means when the action is pause or unstated.
// An UpdateConfig that unticking leaves saying nothing but the defaults is taken out again, so
// ticking and unticking a box on a service that had none changes nothing.
export function setAutoRollback(spec: SpecValue, ticked: boolean): SpecValue {
  if (absent(spec) && !ticked) return spec;
  const base = seedUpdateConfig(spec);
  const action = getPath(base, FAILURE_ACTION);
  if (ticked) return action === "rollback" ? base : setPath(base, FAILURE_ACTION, "rollback");
  if (action !== "rollback" && action !== "continue") return base;
  const paused = setPath(base, FAILURE_ACTION, "pause");
  return isDefaultConfig(getPath(paused, UPDATE_CONFIG))
    ? removePath(paused, UPDATE_CONFIG)
    : paused;
}

export interface AutoRollbackState {
  // The box is ticked: a failed update rolls back.
  checked: boolean;
  // The service continues on failure. The box shows unticked, and says so, because ticking it
  // changes what the service does.
  continues: boolean;
}

// autoRollbackState is how the box reads from a spec.
export function autoRollbackState(spec: unknown): AutoRollbackState {
  let action: unknown;
  try {
    action = getPath(spec, FAILURE_ACTION);
  } catch {
    action = undefined;
  }
  return { checked: action === "rollback", continues: action === "continue" };
}
