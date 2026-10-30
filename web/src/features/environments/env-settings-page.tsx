// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useRouter } from "@tanstack/react-router";
import { useRef, useState } from "react";

import { tokenStatusQuery } from "@/api/agents";
import { type EnvironmentItem, environmentsQuery } from "@/api/environments";
import { NoPermission } from "@/components/full-page-state";
import { StatusDot, useHealthLabel } from "@/components/status-dot";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { AdoptClusterDialog } from "@/features/environments/adopt-cluster-dialog";
import { AgentUpdatesCard } from "@/features/environments/agent-updates-card";
import { ENV_TITLE_ID } from "@/features/environments/env-settings-layout";
import { RenameDialog } from "@/features/environments/environments-settings";
import { FoldCard } from "@/features/environments/fold-card";
import { InstallDialog } from "@/features/environments/install-dialog";
import { JoinTokenCard } from "@/features/environments/join-token-card";
import { NodeTable } from "@/features/environments/node-table";
import { ReasonText } from "@/features/environments/reason-text";
import { RemoveEnvironmentCard } from "@/features/environments/remove-environment-card";
import { SettingsCard } from "@/features/environments/settings-card";
import { RestartLoopCard } from "@/features/restart-loop/restart-loop-card";
import { settingsQuery } from "@/features/settings/general-form";
import { hasPermission, useMe } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

interface ActiveReason {
  code: string;
  node?: string;
  since?: string;
  // How many more problems there are than the one shown.
  more: number;
}

// activeReason is the one reason the Connection card explains: the status's own
// code, else the worst issue of a degraded environment (the server lists them
// worst first). The local socket's codes (unreachable, api_too_old and the
// rest) have wording in ReasonText too.
function activeReason(env: EnvironmentItem): ActiveReason | undefined {
  if (env.status.state === "online") return undefined;
  if (env.status.reason) return { code: env.status.reason, since: env.status.since, more: 0 };
  const issues = env.status.issues ?? [];
  const [first] = issues;
  if (!first) return undefined;
  return {
    code: first.code,
    node: first.node,
    since: first.since,
    more: issues.length - 1,
  };
}

function GeneralCard({ env }: { env: EnvironmentItem }) {
  const t = useT();
  const [renaming, setRenaming] = useState(false);
  const trigger = useRef<HTMLButtonElement>(null);
  return (
    <SettingsCard title={t("envsettings.sections.general")}>
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="flex min-w-0 flex-col gap-1">
          <span className="text-xs font-semibold text-muted-foreground">
            {t("envsettings.general.name")}
          </span>
          <span className="min-w-0 text-sm break-words">{env.name}</span>
        </div>
        <Button
          ref={trigger}
          type="button"
          variant="outline"
          className={`${TOUCH_HEIGHT} max-md:w-full`}
          aria-label={t("envsettings.general.renameAria", { name: env.name })}
          onClick={() => setRenaming(true)}
        >
          {t("envsettings.general.rename")}
        </Button>
      </div>
      {renaming ? (
        <RenameDialog
          key={env.id}
          environment={env}
          returnFocusRef={trigger}
          onClose={() => setRenaming(false)}
        />
      ) : null}
    </SettingsCard>
  );
}

function ConnectionCard({
  env,
  canSettings,
  canConfigure,
}: {
  env: EnvironmentItem;
  canSettings: boolean;
  canConfigure: boolean;
}) {
  const t = useT();
  const [adopting, setAdopting] = useState(false);
  const adoptTrigger = useRef<HTMLButtonElement>(null);
  const healthLabel = useHealthLabel();
  const reason = activeReason(env);
  const isAgent = env.kind === "agent";
  // The address agents dial is a setting, so it is read only for someone who
  // may read settings, and it means nothing for the local socket.
  const settings = useQuery({ ...settingsQuery, enabled: canSettings && isAgent });
  const showUrl = canSettings && isAgent;
  const endpoint = settings.data?.agentEndpoint;
  return (
    <SettingsCard title={t("envsettings.sections.connection")}>
      <dl className="grid gap-x-6 gap-y-3 text-sm sm:grid-cols-[8rem_minmax(0,1fr)]">
        <dt className="text-xs font-semibold text-muted-foreground">
          {t("envsettings.connection.health")}
        </dt>
        <dd>
          <StatusDot state={env.status.state} label={healthLabel(env.status)} />
        </dd>
        {showUrl ? (
          <>
            <dt className="text-xs font-semibold text-muted-foreground">
              {t("envsettings.connection.connectsTo")}
            </dt>
            <dd className="min-w-0 font-mono text-xs break-all">
              {settings.isPending ? (
                <Skeleton className="h-4 w-64" />
              ) : (
                endpoint || t("envsettings.nodes.missing")
              )}
            </dd>
          </>
        ) : null}
      </dl>
      {reason ? (
        <div className="flex flex-col gap-2">
          <ReasonText
            code={reason.code}
            params={{ node: reason.node, envType: env.envType }}
            since={reason.since}
          />
          {reason.more > 0 ? (
            <p className="text-xs text-muted-foreground">
              {t("envsettings.connection.moreIssues", { count: reason.more })}
            </p>
          ) : null}
        </div>
      ) : null}
      {env.status.state === "cluster_changed" && canConfigure ? (
        <>
          <div>
            <Button
              ref={adoptTrigger}
              type="button"
              className={`${TOUCH_HEIGHT} max-md:w-full`}
              onClick={() => setAdopting(true)}
            >
              {t("envsettings.adopt.button")}
            </Button>
          </div>
          <AdoptClusterDialog
            environment={env}
            open={adopting}
            onOpenChange={setAdopting}
            returnFocusRef={adoptTrigger}
          />
        </>
      ) : null}
    </SettingsCard>
  );
}

interface EnvSettingsPageProps {
  envId: string;
}

// EnvSettingsPage is the Settings tab of Settings > Environments > {env} (D-38): the
// cards General, Connection, Nodes and agents, Join token, Agent updates and Restart-loop
// detection (plan 06-35), and
// Remove environment last (D-30). The breadcrumb, the title and the tab bar
// (Settings, Access) belong to EnvSettingsLayout around it, which also answers
// while the environment loads, is gone or is not the caller's to open. The page is
// for someone who holds
// environments.configure on this environment, an env Admin or an instance admin
// (D-07). General, Connection, Nodes and agents and Agent updates are theirs;
// Join token, Local host and Remove environment need the installation-wide
// environments.token, environments.manage and environments.delete, so an env
// Admin never sees those cards (D-19: a card with no usable action is hidden whole).
// The Connection card offers Adopt new cluster while the swarm was
// re-initialised (plan 19); the Local host card (plan 30) comes between Agent updates and
// Remove, only while the server offers the fold. Hiding a card is not access control; the server answers 403.
export function EnvSettingsPage({ envId }: EnvSettingsPageProps) {
  const t = useT();
  const queryClient = useQueryClient();
  const router = useRouter();
  // The toast after a revoke offers to rotate the join token; the install
  // dialog it opens belongs to the page so the toast outlives the table's row.
  const [rotating, setRotating] = useState(false);
  const { data: me } = useMe();
  const environments = useQuery(environmentsQuery);
  const items = environments.data?.items ?? [];
  const env = items.find((e) => e.id === envId);

  // Rename, the auto-update switch, agent updates and adopting a changed cluster need
  // configure on this environment (D-07), so an env Admin has them without holding any
  // installation-wide permission. Revoke is held per environment too.
  const canConfigure = hasPermission(me, "environments.configure", envId);
  const canRevoke = hasPermission(me, "environments.revoke", envId);
  const canRead = hasPermission(me, "environments.read", envId);
  // The rest is installation-wide: the join token, the local-host fold, removal and
  // the settings that hold the address agents dial.
  const canToken = hasPermission(me, "environments.token");
  const canFold = hasPermission(me, "environments.manage");
  const canSettings = hasPermission(me, "settings.read");
  const canDelete = hasPermission(me, "environments.delete");

  if (!canConfigure) return <NoPermission />;
  // The layout has shown the environment loading or gone; there is nothing to add.
  if (!env) return null;

  return (
    <div className="flex flex-col gap-8">
      {canConfigure ? <GeneralCard env={env} /> : null}
      {canRead ? (
        <ConnectionCard env={env} canSettings={canSettings} canConfigure={canConfigure} />
      ) : null}
      {canRead && (canConfigure || canRevoke) ? (
        <SettingsCard title={t("envsettings.sections.nodes")}>
          <NodeTable
            envId={env.id}
            onRotateToken={canToken ? () => setRotating(true) : undefined}
          />
        </SettingsCard>
      ) : null}
      {canToken && env.kind === "agent" ? <JoinTokenCard envId={env.id} /> : null}
      {canConfigure && canRead && env.kind === "agent" ? <AgentUpdatesCard env={env} /> : null}
      {canConfigure ? <RestartLoopCard envId={env.id} /> : null}
      {canToken && env.kind === "agent" ? (
        <InstallDialog
          open={rotating}
          mode="rotate"
          envId={env.id}
          onOpenChange={(open) => {
            setRotating(open);
            // A rotation re-enables enrolment and makes a new token.
            if (!open)
              void queryClient.invalidateQueries({ queryKey: tokenStatusQuery(env.id).queryKey });
          }}
        />
      ) : null}
      {canFold && env.foldOffered ? (
        <FoldCard
          env={env}
          // The card and its dialog are gone with the offer, so focus would fall to the
          // body; the title (in the layout) is where the page starts again.
          focusFallback={() => document.getElementById(ENV_TITLE_ID)}
          onFolded={(swarmEnvId) => {
            // The local host's own page is for an environment that no longer exists.
            if (swarmEnvId !== env.id)
              void router.navigate({
                to: "/settings/environments/$envId",
                params: { envId: swarmEnvId },
              });
          }}
        />
      ) : null}
      {canDelete ? <RemoveEnvironmentCard env={env} /> : null}
    </div>
  );
}
