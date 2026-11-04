// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";

import { environmentsQuery } from "@/api/environments";
import type { UpdateInfo } from "@/api/updates";
import { useEnvName } from "@/features/resources/use-env-name";
import { reasonText } from "@/features/updates/reasons";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";

// The states an Update is shown for: the one it works in, and the ones that say why it cannot yet.
// A workload that is up to date, skipped or ignored has nothing to update to and shows no Update.
const OFFERED: ReadonlySet<UpdateInfo["state"]> = new Set([
  "update_available",
  "busy",
  "updating",
  "pinned_in_file",
  "not_from_registry",
  "not_checked",
  "check_failed",
]);

// The states of an environment in which TierPlan hears from it. An environment whose state is not
// yet known (the local socket before its first probe) is given the benefit of the doubt: the server
// refuses what cannot be done.
const REPORTING: ReadonlySet<string> = new Set(["online", "degraded", "unknown"]);

export interface UpdateGate {
  // Whether the update is shown at all for this state.
  shown: boolean;
  // Why it cannot be used now, in the words of the disabled-with-reason pattern (D-10, UI-SPEC);
  // absent when it can.
  reason?: string;
  // True when it cannot be used and there is nothing to say yet, because the caller's role is not
  // known: the control is disabled outright.
  unavailable: boolean;
}

// useUpdateGate decides what an Update control for a service does, from the permission the caller
// holds (services.control, Operator), whether the environment is reporting, and the state the
// server reports for the image. It only decides what to show; the server enforces every one of these
// (it refuses a moved digest, a held lock, a pinned image and the rest). Ignore under a lock is not
// refused by the server; it is the control's to disable.
export function useUpdateGate(envId: string, update: UpdateInfo | undefined): UpdateGate {
  const t = useT();
  const needs = useNeeds("services.control", envId);
  const envName = useEnvName(envId);
  const environments = useQuery(environmentsQuery);
  const status = environments.data?.items?.find((e) => e.id === envId)?.status?.state;
  const state = update?.state;
  if (!state || !OFFERED.has(state)) return { shown: false, unavailable: false };

  if (!needs.allowed) {
    return needs.reason
      ? {
          shown: true,
          reason: t("updates.menu.needs", { needs: needs.reason, environment: envName }),
          unavailable: false,
        }
      : { shown: true, unavailable: true };
  }
  if (status !== undefined && !REPORTING.has(status)) {
    return {
      shown: true,
      reason: t("updates.menu.disabled.offline", { environment: envName }),
      unavailable: false,
    };
  }
  switch (state) {
    case "busy":
      return {
        shown: true,
        reason: t("updates.menu.disabled.busy", { what: holderWords(t, update, envName) }),
        unavailable: false,
      };
    case "updating":
      return { shown: true, reason: t("updates.menu.disabled.updating"), unavailable: false };
    case "check_failed":
      return {
        shown: true,
        reason: t("updates.menu.disabled.check_failed", {
          reason: reasonText(t, update.reason, update.registry, update.repository, update.tag),
        }),
        unavailable: false,
      };
    case "pinned_in_file":
    case "not_from_registry":
    case "not_checked":
      return { shown: true, reason: t(`updates.menu.disabled.${state}`), unavailable: false };
    default:
      return { shown: true, unavailable: false };
  }
}

// holderWords is who holds the environment, as the tooltip of a waiting Update says it.
function holderWords(
  t: (key: string, options?: Record<string, unknown>) => string,
  update: UpdateInfo,
  envName: string,
): string {
  const holder = update.holder;
  if (holder?.kind === "janitor") {
    return t("updates.menu.disabled.busyJanitor", { environment: envName });
  }
  if (holder?.kind === "update" && holder.workload) {
    return t("updates.menu.disabled.busyUpdate", { workload: holder.workload });
  }
  if (holder?.stack) return t("updates.menu.disabled.busyStack", { stack: holder.stack });
  return t("updates.menu.disabled.busyOther", { environment: envName });
}
