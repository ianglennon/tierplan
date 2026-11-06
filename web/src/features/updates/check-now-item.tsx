// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useCheckingUpdates, useCheckUpdates } from "@/api/updates";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { DropdownMenuItem } from "@/components/ui/dropdown-menu";
import { useEnvName } from "@/features/resources/use-env-name";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";

interface CheckNowItemProps {
  envId: string;
}

// CheckNowItem is "Check now" in a row menu: it asks the registries again for every image of the
// environment (IMG-05), not only the row's. It needs updates.check (Operator); below that it is
// unavailable with the reason (D-19) and the server decides either way. While the check runs it
// reads "Checking…" and is busy, so it cannot be pressed twice; the result is a polite toast, sent
// by the mutation since the menu is gone before the answer.
export function CheckNowItem({ envId }: CheckNowItemProps) {
  const t = useT();
  const needs = useNeeds("updates.check", envId);
  const envName = useEnvName(envId);
  const check = useCheckUpdates(envId);
  const checking = useCheckingUpdates(envId);
  const reason = needs.reason
    ? t("updates.check.needs", { needs: needs.reason, environment: envName })
    : undefined;
  return (
    <NeedsTooltip reason={reason}>
      <DropdownMenuItem
        className="max-md:min-h-11"
        disabled={!needs.allowed && !needs.reason}
        aria-busy={checking || undefined}
        onSelect={(event) => {
          if (!needs.allowed) return;
          // A second press while the first is out does nothing; nothing is sent.
          if (checking) {
            event.preventDefault();
            return;
          }
          check.mutate();
        }}
      >
        {checking ? t("updates.check.pending") : t("updates.check.action")}
      </DropdownMenuItem>
    </NeedsTooltip>
  );
}
