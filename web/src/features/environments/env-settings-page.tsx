// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useRouter } from "@tanstack/react-router";
import { Server } from "lucide-react";
import { useRef, useState } from "react";

import { tokenStatusQuery } from "@/api/agents";
import { type EnvironmentItem, environmentsQuery } from "@/api/environments";
import { NoPermission } from "@/components/full-page-state";
import { StatusDot, useHealthLabel } from "@/components/status-dot";
import { TypeBadge } from "@/components/type-badge";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@/components/ui/empty";
import { Skeleton } from "@/components/ui/skeleton";
import { AdoptClusterDialog } from "@/features/environments/adopt-cluster-dialog";
import { AgentUpdatesCard } from "@/features/environments/agent-updates-card";
import { RenameDialog } from "@/features/environments/environments-settings";
import { FoldCard } from "@/features/environments/fold-card";
import { InstallDialog } from "@/features/environments/install-dialog";
import { JoinTokenCard } from "@/features/environments/join-token-card";
import { NodeTable } from "@/features/environments/node-table";
import { ReasonText } from "@/features/environments/reason-text";
import { RemoveEnvironmentCard } from "@/features/environments/remove-environment-card";
import { SettingsCard } from "@/features/environments/settings-card";
import { settingsQuery } from "@/features/settings/general-form";
import { hasPermission, useDocumentTitle, useMe } from "@/hooks/use-me";
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

function Breadcrumb({ name }: { name: string }) {
  const t = useT();
  return (
    <nav aria-label={t("envsettings.breadcrumb.label")}>
      <ol className="flex flex-wrap items-center gap-x-2 text-xs text-muted-foreground [&>li:not(:last-child)]:after:ms-2 [&>li:not(:last-child)]:after:content-['/']">
        <li>
          <Link to="/settings" className="rounded-sm text-accent-ink hover:underline">
            {t("envsettings.breadcrumb.settings")}
          </Link>
        </li>
        <li>
          <Link to="/settings/environments" className="rounded-sm text-accent-ink hover:underline">
            {t("envsettings.breadcrumb.environments")}
          </Link>
        </li>
        <li className="min-w-0">
          <span aria-current="page" className="break-words">
            {name}
          </span>
        </li>
      </ol>
    </nav>
  );
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
  canManage,
}: {
  env: EnvironmentItem;
  canSettings: boolean;
  canManage: boolean;
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
      {env.status.state === "cluster_changed" && canManage ? (
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

// EnvSettingsPage is Settings > Environments > {env} (D-38): the breadcrumb and
// the cards General, Connection, Nodes and agents, Join token and Agent updates,
// and Remove environment last (D-30), each shown only to someone who holds its
// permission. The Connection card offers Adopt new cluster while the swarm was
// re-initialised (plan 19); the Local host card (plan 30) comes between Agent updates and
// Remove, only while the server offers the fold. Hiding a card is not access control; the server answers 403.
export function EnvSettingsPage({ envId }: EnvSettingsPageProps) {
  const t = useT();
  const queryClient = useQueryClient();
  const router = useRouter();
  // The title takes focus after a fold, when the card that had it is gone.
  const title = useRef<HTMLHeadingElement>(null);
  // The toast after a revoke offers to rotate the join token; the install
  // dialog it opens belongs to the page so the toast outlives the table's row.
  const [rotating, setRotating] = useState(false);
  const { data: me } = useMe();
  const environments = useQuery(environmentsQuery);
  const items = environments.data?.items ?? [];
  const env = items.find((e) => e.id === envId);
  useDocumentTitle(env?.name);

  const canManage = hasPermission(me, "environments.manage");
  const canToken = hasPermission(me, "environments.token");
  const canRevoke = hasPermission(me, "environments.revoke");
  const canRead = hasPermission(me, "environments.read", envId);
  const canSettings = hasPermission(me, "settings.read");
  const canDelete = hasPermission(me, "environments.delete");

  if (!(canManage || canToken || canRevoke || canSettings || canDelete)) return <NoPermission />;

  if (environments.isPending) {
    return (
      <div className="flex flex-col gap-4" aria-busy="true">
        <span role="status" className="sr-only">
          {t("envsettings.loading")}
        </span>
        <Skeleton className="h-8 w-64" />
        <Skeleton className="h-32 w-full" />
      </div>
    );
  }

  if (environments.isError) {
    return (
      <Alert variant="destructive">
        <AlertDescription className="flex flex-wrap items-center gap-3">
          <span>{t("envsettings.loadFailed")}</span>
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="max-md:h-11"
            disabled={environments.isFetching}
            onClick={() => void environments.refetch()}
          >
            {t("envsettings.retry")}
          </Button>
        </AlertDescription>
      </Alert>
    );
  }

  if (!env) {
    return (
      <Empty>
        <EmptyHeader>
          <EmptyMedia variant="icon">
            <Server aria-hidden="true" />
          </EmptyMedia>
          <EmptyTitle>{t("envsettings.notFound.heading")}</EmptyTitle>
          <EmptyDescription>{t("envsettings.notFound.body")}</EmptyDescription>
        </EmptyHeader>
        <EmptyContent>
          <Button asChild variant="outline" className="max-md:h-11">
            <Link to="/settings/environments">{t("envsettings.notFound.back")}</Link>
          </Button>
        </EmptyContent>
      </Empty>
    );
  }

  return (
    <div className="flex flex-col gap-8">
      <header className="flex flex-col gap-2">
        <Breadcrumb name={env.name} />
        <div className="flex flex-wrap items-center gap-2">
          <h2
            ref={title}
            tabIndex={-1}
            className="min-w-0 text-2xl font-semibold break-words outline-none"
          >
            {env.name}
          </h2>
          <TypeBadge envType={env.envType} type={env.type} isLocalSocket={env.isLocalSocket} />
          {env.isSelfHost ? (
            <Badge variant="outline" className="shrink-0">
              {t("dashboard.selfBadge")}
            </Badge>
          ) : null}
        </div>
      </header>
      {canManage ? <GeneralCard env={env} /> : null}
      {canRead ? (
        <ConnectionCard env={env} canSettings={canSettings} canManage={canManage} />
      ) : null}
      {canRead ? (
        <SettingsCard title={t("envsettings.sections.nodes")}>
          <NodeTable
            envId={env.id}
            onRotateToken={canToken ? () => setRotating(true) : undefined}
          />
        </SettingsCard>
      ) : null}
      {canToken && env.kind === "agent" ? <JoinTokenCard envId={env.id} /> : null}
      {canManage && canRead && env.kind === "agent" ? <AgentUpdatesCard env={env} /> : null}
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
      {canManage && env.foldOffered ? (
        <FoldCard
          env={env}
          // The card and its dialog are gone with the offer, so focus would fall to the
          // body; the title is where the page starts again.
          focusFallback={() => title.current}
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
