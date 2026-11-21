// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Why a service cannot be scaled: a global service runs one task per node and a job runs to
// completion, so neither has a replica count to set. TierPlan's ingress proxy is its own and
// is never changed from here (D-29).
export type ScaleSkip = "global" | "job" | "ingress";

export interface ScaleEligibility {
  // True for a replicated service, the only kind with a replica count (D-10).
  eligible: boolean;
  // Set when it is not eligible and the reason has words ("Global services can't be scaled.").
  note: ScaleSkip | undefined;
  // The stack the service belongs to, when it has one: a scale of it is undone by the next
  // stack deploy, which the dialog says.
  stack: string | undefined;
}

// scaleEligibility says whether a service can be scaled and, when it cannot, which note the
// bulk dialog gives it. The server decides either way; this only keeps the dialog honest
// about what it will send. The ingress proxy is skipped whatever its mode, with its own note.
export function scaleEligibility(row: {
  mode: string;
  stack: string;
  ingress?: boolean;
}): ScaleEligibility {
  const stack = row.stack ? row.stack : undefined;
  if (row.ingress) return { eligible: false, note: "ingress", stack };
  switch (row.mode) {
    case "replicated":
      return { eligible: true, note: undefined, stack };
    case "global":
      return { eligible: false, note: "global", stack };
    case "replicated-job":
    case "global-job":
      return { eligible: false, note: "job", stack };
    default:
      return { eligible: false, note: undefined, stack };
  }
}
