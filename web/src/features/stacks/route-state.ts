// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import type { HealthState } from "@/api/environments";
import type { RouteCondition } from "@/api/ingress";
import type { StackRouteState } from "@/api/stack-routes";
import { keyExists } from "@/i18n/use-t";

// RouteHeadlineState is every state a route can show. "unknown" is the environment routes endpoint's seventh state,
// used only when the engine could not be read.
export type RouteHeadlineState = StackRouteState | "unknown";

// routeHeadline is the one map from a route's state to the dot colour and the label it shows, shared by the stack
// page's Routes card and the Ingress page, so the two cannot disagree. Status colours are for health only, and every
// dot carries its label, so no state is colour alone. A stopped stack's routes are grey (it is stopped on purpose);
// a started stack with no running task is a fault, so red. A state the engine could not read is grey, never a
// healthy colour.
export function routeHeadline(
  state: RouteHeadlineState,
  stopped: boolean,
): { dot: HealthState; labelKey: string } {
  return { dot: dotOf(state, stopped), labelKey: `stackIngress.routes.state.${state}.label` };
}

function dotOf(state: RouteHeadlineState, stopped: boolean): HealthState {
  switch (state) {
    case "serving":
      return "online";
    case "pending":
      return "reconnecting";
    case "waiting_certificate":
    case "not_attached":
      return "degraded";
    case "backend_not_running":
      return stopped ? "unknown" : "offline";
    case "ingress_off":
      return "pending";
    case "unknown":
      return "unknown";
  }
}

// waitingCertificateDetail is the line under "Waiting for certificate", shared by the stack page's Routes card and the
// Ingress page so the two say the same. A certificate whose last order failed says so, then the failure's hint as the
// server sent it on the certificate condition; any other known reason has its own sentence, and an unknown one is drawn
// as sent.
export function waitingCertificateDetail(
  reason: string,
  conditions: RouteCondition[] | null | undefined,
  t: (key: string, options?: Record<string, unknown>) => string,
): string {
  if (reason === "") return "";
  const cert = (conditions ?? []).find((c) => c.code === "certificate_ready");
  if (reason === "certificate_not_ready" && cert?.failed && cert.message !== "") {
    return t("stackIngress.routes.reason.certificate_failed", { hint: cert.message });
  }
  const key = `stackIngress.routes.reason.${reason}`;
  return keyExists(key) ? t(key) : reason;
}
