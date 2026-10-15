// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { Link, Outlet, useNavigate } from "@tanstack/react-router";
import { LockKeyhole } from "lucide-react";
import { useState } from "react";

import { isSecretGone, isSecretNotASwarm, secretDetailQuery } from "@/api/secrets";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
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
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { UsedByList } from "@/components/used-by-list";
import { useEnvName } from "@/features/resources/use-env-name";
import { DeleteSecretDialog, secretDeleteReason } from "@/features/secrets/delete-secret-dialog";
import { RotateDialog } from "@/features/secrets/rotate-dialog";
import { NotASwarm } from "@/features/services/not-a-swarm";
import { useDocumentTitle } from "@/hooks/use-me";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";

interface SecretDetailLayoutProps {
  envId: string;
  secretId: string;
}

// The tabs of the page, in the order they show. Each is a child route of the page under
// secrets_.$secretId.*; the Overview is the page's own address.
const TABS = [
  {
    id: "overview",
    to: "/env/$envId/secrets/$secretId",
    labelKey: "secrets.detail.tabs.overview",
    exact: true,
  },
  {
    id: "used-by",
    to: "/env/$envId/secrets/$secretId/used-by",
    labelKey: "secrets.detail.tabs.usedBy",
    exact: false,
  },
  {
    id: "inspect",
    to: "/env/$envId/secrets/$secretId/inspect",
    labelKey: "secrets.detail.tabs.inspect",
    exact: false,
  },
] as const;

function Breadcrumb({ envId, name }: { envId: string; name: string | undefined }) {
  const t = useT();
  return (
    <nav aria-label={t("secrets.detail.breadcrumb.label")}>
      <ol className="flex flex-wrap items-center gap-x-2 text-xs text-muted-foreground [&>li:not(:last-child)]:after:ms-2 [&>li:not(:last-child)]:after:content-['/']">
        <li>
          <Link
            to="/env/$envId/secrets"
            params={{ envId }}
            className="rounded-sm text-accent-ink hover:underline"
          >
            {t("secrets.detail.breadcrumb.secrets")}
          </Link>
        </li>
        <li className="min-w-0">
          {name === undefined ? (
            <Skeleton className="h-3 w-24" />
          ) : (
            <span aria-current="page" className="break-all font-mono">
              {name}
            </span>
          )}
        </li>
      </ol>
    </nav>
  );
}

// Gone is what a secret's page becomes when the secret no longer exists: it was removed, or a
// new version replaced it. It replaces the page, tabs included.
function Gone({ envId, name }: { envId: string; name: string | undefined }) {
  const t = useT();
  return (
    <div className="flex flex-col gap-6">
      <header>
        <Breadcrumb envId={envId} name={name} />
      </header>
      <Empty>
        <EmptyHeader>
          <EmptyMedia variant="icon">
            <LockKeyhole aria-hidden="true" />
          </EmptyMedia>
          <EmptyTitle role="heading" aria-level={2}>
            {t("secrets.detail.gone.heading")}
          </EmptyTitle>
          <EmptyDescription>{t("secrets.detail.gone.body")}</EmptyDescription>
        </EmptyHeader>
        <EmptyContent>
          <Button asChild variant="outline" className="max-md:h-11">
            <Link to="/env/$envId/secrets" params={{ envId }}>
              {t("secrets.detail.gone.back")}
            </Link>
          </Button>
        </EmptyContent>
      </Empty>
    </div>
  );
}

// SecretDetailLayout is the frame of a secret's page: the breadcrumb, the header with the name,
// how many services use it and Delete…, the in-use callout while any service does, the tab bar
// (Link triggers, the active tab follows the URL) and the tab under it. The tabs render at once,
// before the secret has loaded (UI-SPEC "Loading"). Nothing on the page is a value.
export function SecretDetailLayout({ envId, secretId }: SecretDetailLayoutProps) {
  const t = useT();
  const navigate = useNavigate();
  const envName = useEnvName(envId);
  const manage = useNeeds("secrets.manage", envId);
  const control = useNeeds("services.control", envId);
  const rotateReason = manage.reason ?? control.reason;
  const [deleting, setDeleting] = useState(false);
  const [rotating, setRotating] = useState(false);
  const query = useQuery(secretDetailQuery(envId, secretId));
  const { data } = query;
  const gone = isSecretGone(query.error);
  useDocumentTitle(gone ? t("secrets.detail.gone.heading") : data?.name);

  if (isSecretNotASwarm(query.error)) return <NotASwarm envId={envId} envName={envName} />;
  if (gone) return <Gone envId={envId} name={data?.name} />;

  if (query.isError && !data) {
    return (
      <div className="flex flex-col gap-6">
        <header>
          <Breadcrumb envId={envId} name={secretId} />
        </header>
        <Alert variant="destructive">
          <AlertDescription className="flex flex-wrap items-center gap-3">
            <span>{t("secrets.detail.error.loadFailed", { name: secretId })}</span>
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="max-md:h-11"
              disabled={query.isFetching}
              onClick={() => void query.refetch()}
            >
              {t("secrets.detail.error.retry")}
            </Button>
          </AlertDescription>
        </Alert>
      </div>
    );
  }

  const reason = data ? secretDeleteReason(t, manage.reason, data.usedBy, true) : undefined;
  const rows = data?.usedByRows ?? [];
  return (
    <div className="flex flex-col gap-6">
      <header className="flex flex-col gap-2">
        <Breadcrumb envId={envId} name={data?.name} />
        {data ? (
          <div className="flex flex-wrap items-start justify-between gap-3">
            <div className="flex min-w-0 flex-col gap-1">
              <Tooltip>
                <TooltipTrigger asChild>
                  <h2 className="max-w-full min-w-0 truncate font-mono text-2xl font-semibold">
                    {data.name}
                  </h2>
                </TooltipTrigger>
                <TooltipContent className="break-all font-mono">{data.name}</TooltipContent>
              </Tooltip>
              <p className="text-sm text-muted-foreground">
                {data.usedBy > 0
                  ? t("secrets.detail.usedBy", { count: data.usedBy })
                  : t("secrets.detail.notInUse")}
              </p>
            </div>
            <div className="flex flex-wrap items-center gap-2">
              <NeedsTooltip reason={rotateReason}>
                <Button
                  type="button"
                  variant="outline"
                  className="max-md:h-11"
                  aria-label={t("secrets.rotate.label", { name: data.name })}
                  onClick={rotateReason ? undefined : () => setRotating(true)}
                >
                  {t("secrets.rotate.action")}
                </Button>
              </NeedsTooltip>
              <NeedsTooltip reason={reason}>
                <Button
                  type="button"
                  variant="outline"
                  className="max-md:h-11 border-destructive-fill text-destructive"
                  aria-label={t("secrets.delete.label", { name: data.name })}
                  onClick={reason ? undefined : () => setDeleting(true)}
                >
                  {t("secrets.delete.action")}
                </Button>
              </NeedsTooltip>
            </div>
          </div>
        ) : (
          <Skeleton className="h-8 w-64" />
        )}
      </header>
      {data && data.usedBy > 0 ? (
        <Alert variant="warning">
          <AlertTitle>{t("secrets.detail.inUse.title", { name: data.name })}</AlertTitle>
          <AlertDescription className="flex flex-col gap-2">
            <p>{t("secrets.detail.inUse.body")}</p>
            <UsedByList
              containers={[]}
              services={rows.map((r) => ({
                name: r.service,
                file: r.file,
                uid: r.uid,
                gid: r.gid,
                mode: r.mode,
              }))}
            />
          </AlertDescription>
        </Alert>
      ) : null}
      <Tabs value="" className="gap-4">
        <TabsList
          variant="line"
          aria-label={t("secrets.detail.tabs.label")}
          className="h-9 max-md:h-11"
        >
          {TABS.map((tab) => (
            <TabsTrigger
              key={tab.id}
              value={tab.id}
              asChild
              className="data-active:text-accent-ink after:bg-accent-ink max-md:h-11"
            >
              <Link
                to={tab.to}
                params={{ envId, secretId }}
                activeOptions={{ exact: tab.exact }}
                activeProps={{ "data-active": true }}
                inactiveProps={{ "data-active": undefined }}
              >
                {t(tab.labelKey)}
              </Link>
            </TabsTrigger>
          ))}
        </TabsList>
      </Tabs>
      <Outlet />
      {data && manage.allowed && control.allowed ? (
        <RotateDialog
          kind="secret"
          envId={envId}
          object={rotating ? { id: data.id, name: data.name } : null}
          onClose={() => setRotating(false)}
        />
      ) : null}
      {data && manage.allowed ? (
        <DeleteSecretDialog
          envId={envId}
          secret={deleting ? { id: data.id, name: data.name } : null}
          onClose={() => setDeleting(false)}
          onDeleted={() => void navigate({ to: "/env/$envId/secrets", params: { envId } })}
        />
      ) : null}
    </div>
  );
}
