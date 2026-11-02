// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useId, useMemo, useState } from "react";

import {
  type AgentItem,
  agentsQuery,
  isUpdating,
  refreshAfterAgentUpdate,
  setAutoUpdate,
  UpdateActionError,
  updateAllAgents,
} from "@/api/agents";
import { type EnvironmentItem, environmentsQuery } from "@/api/environments";
import { CopyableCode } from "@/components/copyable-code";
import { Button } from "@/components/ui/button";
import { Label } from "@/components/ui/label";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { Switch } from "@/components/ui/switch";
import { SettingsCard } from "@/features/environments/settings-card";
import { updateProblemKey } from "@/features/environments/update-problem";
import { usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { toastError } from "@/lib/toast";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

interface AgentUpdatesCardProps {
  env: EnvironmentItem;
}

// AgentUpdatesCard is the "Agent updates" section (D-24, D-28): the switch
// "Update agents automatically", and, while it is off, Update all agents ({n}) and
// the progress "Updating agents ({done} of {total})" while an update rolls through
// the nodes. A host whose owner turned self-update off, or whose agent TierPlan
// cannot update, gets its manual command here. It is for someone holding
// environments.configure on this environment (D-07) and only for an agent
// environment; the server checks. Without the permission it renders nothing and
// reads nothing.
export function AgentUpdatesCard({ env }: AgentUpdatesCardProps) {
  const allowed = usePermission("environments.configure", env.id);
  return allowed ? <AgentUpdatesBody env={env} /> : null;
}

function AgentUpdatesBody({ env }: AgentUpdatesCardProps) {
  const t = useT();
  const queryClient = useQueryClient();
  const switchId = useId();
  const helperId = useId();
  const agents = useQuery(agentsQuery(env.id));
  const items = useMemo(() => agents.data?.items ?? [], [agents.data]);
  const active = items.filter((a) => a.status === "active");
  const autoOn = env.autoUpdate !== false;

  const auto = useMutation({
    mutationFn: (on: boolean) => setAutoUpdate(env.id, on),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: environmentsQuery.queryKey }),
    onError: () => toastError(t("envsettings.updates.autoFailed")),
  });
  // The switch shows what was just asked for until the server has answered.
  const checked = auto.isPending ? (auto.variables ?? autoOn) : autoOn;

  const all = useMutation({
    mutationFn: () => updateAllAgents(env.id),
    onSettled: () => refreshAfterAgentUpdate(queryClient, env.id),
    onError: (error) =>
      toastError(t(updateProblemKey(error instanceof UpdateActionError ? error.type : undefined))),
  });

  // Agents that an update would bring up to date and that can take it.
  const candidates = active.filter(
    (a) => a.connected && a.selfUpdate && a.updateAvailable && !isUpdating(a),
  );

  // Every agent seen updating since the last time none was, so the count of "done of
  // total" survives the agents leaving the updating state one by one.
  const [seen, setSeen] = useState<string[]>([]);
  useEffect(() => {
    const ids = items.filter(isUpdating).map((a) => a.id);
    setSeen((prev) => {
      if (ids.length === 0) return prev.length === 0 ? prev : [];
      const next = Array.from(new Set([...prev, ...ids]));
      return next.length === prev.length ? prev : next;
    });
  }, [items]);
  const updating = items.filter(isUpdating);
  const total = Math.max(seen.length, updating.length);
  const done = seen.filter((id) => {
    const a = items.find((x) => x.id === id);
    return (
      a !== undefined && !isUpdating(a) && !a.updateAvailable && a.updateState?.state === "idle"
    );
  }).length;

  const byHand = active.filter((a) => a.manualUpdateCommand);

  return (
    <SettingsCard title={t("envsettings.sections.updates")}>
      <div className="flex items-start gap-3">
        <Switch
          id={switchId}
          checked={checked}
          disabled={auto.isPending}
          aria-describedby={helperId}
          onCheckedChange={(on) => auto.mutate(on)}
        />
        <div className="flex min-w-0 flex-col gap-1">
          <Label htmlFor={switchId}>{t("envsettings.updates.autoLabel")}</Label>
          <p id={helperId} className="text-sm text-muted-foreground">
            {t("envsettings.updates.autoHelper")}
          </p>
        </div>
      </div>

      {!checked ? (
        <div className="flex flex-col gap-3">
          <div>
            <Button
              type="button"
              variant="outline"
              className={`${TOUCH_HEIGHT} max-md:w-full`}
              disabled={all.isPending || candidates.length === 0 || agents.isPending}
              onClick={() => all.mutate()}
            >
              {all.isPending ? (
                <>
                  <Spinner aria-hidden="true" />
                  {t("envsettings.updates.starting")}
                </>
              ) : (
                t("envsettings.updates.updateAll", { count: candidates.length })
              )}
            </Button>
          </div>
          {env.envType === "swarm" ? (
            <p className="text-sm text-muted-foreground">{t("envsettings.updates.swarmNote")}</p>
          ) : null}
        </div>
      ) : null}

      {agents.isPending ? <Skeleton className="h-5 w-64" /> : null}

      <div role="status" className="text-sm">
        {updating.length > 0 ? t("envsettings.updates.progress", { done, total }) : null}
      </div>

      {byHand.map((a) => (
        <ManualUpdate key={a.id} agent={a} />
      ))}
    </SettingsCard>
  );
}

// ManualUpdate is the note and command block of a node TierPlan cannot update: its
// host owner turned self-update off, or the agent is outside the version window or
// too old to update itself.
function ManualUpdate({ agent }: { agent: AgentItem }) {
  const t = useT();
  const hostRefuses = agent.connected && !agent.selfUpdate;
  return (
    <div className="flex min-w-0 flex-col gap-2">
      <p className="text-sm">
        {t(hostRefuses ? "envsettings.updates.manualOff" : "envsettings.updates.manualOther", {
          node: agent.node,
        })}
      </p>
      <CopyableCode
        code={agent.manualUpdateCommand ?? ""}
        label={t("envsettings.updates.manualLabel", { node: agent.node })}
      />
    </div>
  );
}
