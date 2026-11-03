// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Ban, Info } from "lucide-react";
import { useId } from "react";

import { useSetStackJanitor } from "@/api/janitor";
import type { StackDetail } from "@/api/stacks";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Label } from "@/components/ui/label";
import { Spinner } from "@/components/ui/spinner";
import { Switch } from "@/components/ui/switch";
import { SettingsCard } from "@/features/environments/settings-card";
import { useEnvName } from "@/features/resources/use-env-name";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { toastError, toastSuccess } from "@/lib/toast";

interface StackJanitorCardProps {
  envId: string;
  stack: StackDetail;
}

// StackJanitorCard is the stack's "Janitor" card on its Overview (D-10, anchor #janitor): one switch
// that leaves everything the stack owns alone, for a stack whose old containers a person wants to keep.
// It applies at once, so there is no Save: the switch shows the answer while it is out (with a spinner)
// and goes back, with the error announced, when the server refuses. A compose file that says
// `x-tierplan: {keep: true}` keeps the stack out of every cleanup too; the switch then reads on and
// cannot be turned off here, because only the file can undo that. It needs janitor.policy, as the
// policy does (D-15, D-21).
export function StackJanitorCard({ envId, stack }: StackJanitorCardProps) {
  const t = useT();
  const switchId = useId();
  const helperId = useId();
  const envName = useEnvName(envId);
  const needs = useNeeds("janitor.policy", envId);
  const set = useSetStackJanitor(envId, stack.id);

  const declared = stack.janitor.declared;
  // While a change is out the switch shows what was asked for; if it is refused the variable is
  // cleared and the switch is the stack's own state again.
  const on = declared || (set.isPending ? set.variables : stack.janitor.excluded);
  const reason = !needs.allowed
    ? t("janitor.needsOn", { need: needs.reason ?? t("needs.role.admin"), name: envName })
    : declared
      ? t("janitorKeep.stack.declaredReason")
      : undefined;

  async function change(excluded: boolean) {
    if (set.isPending) return;
    try {
      await set.mutateAsync(excluded);
      toastSuccess(
        t(excluded ? "janitorKeep.stack.excluded" : "janitorKeep.stack.included", {
          name: stack.name,
        }),
      );
    } catch {
      toastError(t("janitorKeep.stack.failed", { name: stack.name }));
    }
  }

  return (
    <SettingsCard title={t("janitorKeep.stack.title")} id="janitor">
      {declared ? (
        <Alert role="status">
          <Info aria-hidden="true" />
          <AlertDescription className="text-foreground break-words">
            {t("janitorKeep.stack.declared")}
          </AlertDescription>
        </Alert>
      ) : null}
      <div className="flex items-start justify-between gap-4">
        <div className="flex min-w-0 flex-col gap-1">
          <Label htmlFor={switchId}>{t("janitorKeep.stack.switch")}</Label>
          <p id={helperId} className="text-sm text-muted-foreground">
            {t("janitorKeep.stack.helper")}
          </p>
        </div>
        <div className="flex shrink-0 items-center gap-2">
          {set.isPending ? <Spinner aria-label={t("janitorKeep.stack.saving")} /> : null}
          {on ? (
            <Badge variant="outline">
              <Ban aria-hidden="true" />
              {t("janitorKeep.stack.badge")}
            </Badge>
          ) : null}
          <NeedsTooltip reason={reason}>
            <Switch
              id={switchId}
              checked={on}
              disabled={set.isPending}
              aria-describedby={helperId}
              onCheckedChange={(next) => void change(next)}
            />
          </NeedsTooltip>
        </div>
      </div>
    </SettingsCard>
  );
}
