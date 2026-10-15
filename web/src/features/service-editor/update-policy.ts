// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { getPath, setPath } from "@/features/service-editor/spec-patch";
import type { SpecValue } from "@/features/spec-editor/spec-format";

// The auto-rollback choice (D-12 amended): "Roll back automatically if an update fails" is the
// service's own UpdateConfig.FailureAction, rollback when ticked and pause (Docker's default)
// when not. The Update policy card and the Review step both show it, and both change it here.

const UPDATE_CONFIG = "UpdateConfig";
const FAILURE_ACTION = "UpdateConfig.FailureAction";

// What `docker service create` gives a service that states no update policy. A service with no
// UpdateConfig updates one task at a time, stopping each before starting its replacement; an
// UpdateConfig that says Parallelism 0 updates all of them at once. So the first time the box
// creates an UpdateConfig it has to say what the absent one meant, or ticking it would quietly
// change how fast a rollout goes (RESEARCH Pitfall 5).
export const AUTO_ROLLBACK_DEFAULTS = {
  Parallelism: 1,
  Delay: 0,
  Monitor: 5_000_000_000,
  MaxFailureRatio: 0,
  Order: "stop-first",
} as const;

// setAutoRollback returns the spec with the choice applied and nothing else changed. Ticked on a
// service with no UpdateConfig it creates one with the defaults above; unticked on such a
// service it leaves it absent. On an UpdateConfig that exists, only FailureAction changes, and
// only when it has to: pause is already what unticked means when the action is pause or unstated.
export function setAutoRollback(spec: SpecValue, ticked: boolean): SpecValue {
  const existing = getPath(spec, UPDATE_CONFIG);
  if (existing === undefined || existing === null) {
    if (!ticked) return spec;
    return setPath(spec, UPDATE_CONFIG, { ...AUTO_ROLLBACK_DEFAULTS, FailureAction: "rollback" });
  }
  const action = getPath(spec, FAILURE_ACTION);
  if (ticked) return action === "rollback" ? spec : setPath(spec, FAILURE_ACTION, "rollback");
  return action === "rollback" || action === "continue"
    ? setPath(spec, FAILURE_ACTION, "pause")
    : spec;
}

// seedUpdateConfig returns the spec with an UpdateConfig of the CLI defaults when it has none.
export function seedUpdateConfig(spec: SpecValue): SpecValue {
  return spec;
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
