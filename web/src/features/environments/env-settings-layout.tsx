// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { Link, Outlet } from "@tanstack/react-router";
import { Server } from "lucide-react";

import { environmentsQuery } from "@/api/environments";
import { NoPermission } from "@/components/full-page-state";
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
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { hasPermission, useDocumentTitle, useMe } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

// The environment's title is where the page starts again after a card that had
// focus is gone (a fold), so the page finds it by this ID.
export const ENV_TITLE_ID = "env-settings-title";

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

interface EnvSettingsLayoutProps {
  envId: string;
}

// EnvSettingsLayout is the frame of Settings > Environments > {env} (D-38, D-25):
// the breadcrumb "Settings / Environments / {name}", the title with its badges,
// the tab bar "Settings", "Access" and "Shells" (Link triggers, the active tab follows the
// URL) and the tab under it. It answers while the environment loads, fails to
// load or is gone, so both tabs start from a known environment. The Access tab
// shows only to someone who holds environments.access on this environment, and the
// Shells tab to someone who holds shells.manage, an env Admin or an instance admin for
// both; hiding a tab is not access control, the server
// answers 403 (or 404 for an environment the caller cannot see).
export function EnvSettingsLayout({ envId }: EnvSettingsLayoutProps) {
  const t = useT();
  const { data: me } = useMe();
  const environments = useQuery(environmentsQuery);
  const env = (environments.data?.items ?? []).find((e) => e.id === envId);
  useDocumentTitle(env?.name);

  const canConfigure = hasPermission(me, "environments.configure", envId);
  const canAccess = hasPermission(me, "environments.access", envId);
  const canShells = hasPermission(me, "shells.manage", envId);
  if (!canConfigure && !canAccess) return <NoPermission />;

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
            id={ENV_TITLE_ID}
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
        {canAccess || canShells ? (
          <Tabs value="" className="mt-2 gap-4">
            <TabsList
              variant="line"
              aria-label={t("envaccess.tabs.label")}
              className="h-9 max-md:h-11"
            >
              <TabsTrigger
                value="settings"
                asChild
                className="data-active:text-accent-ink after:bg-accent-ink max-md:h-11"
              >
                <Link
                  to="/settings/environments/$envId"
                  params={{ envId }}
                  activeOptions={{ exact: true }}
                  activeProps={{ "data-active": true }}
                  inactiveProps={{ "data-active": undefined }}
                >
                  {t("envaccess.tabs.settings")}
                </Link>
              </TabsTrigger>
              {canAccess ? (
                <TabsTrigger
                  value="access"
                  asChild
                  className="data-active:text-accent-ink after:bg-accent-ink max-md:h-11"
                >
                  <Link
                    to="/settings/environments/$envId/access"
                    params={{ envId }}
                    activeProps={{ "data-active": true }}
                    inactiveProps={{ "data-active": undefined }}
                  >
                    {t("envaccess.tabs.access")}
                  </Link>
                </TabsTrigger>
              ) : null}
              {canShells ? (
                <TabsTrigger
                  value="shells"
                  asChild
                  className="data-active:text-accent-ink after:bg-accent-ink max-md:h-11"
                >
                  <Link
                    to="/settings/environments/$envId/shells"
                    params={{ envId }}
                    activeProps={{ "data-active": true }}
                    inactiveProps={{ "data-active": undefined }}
                  >
                    {t("activeshells.tab")}
                  </Link>
                </TabsTrigger>
              ) : null}
            </TabsList>
          </Tabs>
        ) : null}
      </header>
      <Outlet />
    </div>
  );
}
