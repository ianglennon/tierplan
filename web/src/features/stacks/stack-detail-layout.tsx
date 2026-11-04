// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import { Link, Outlet } from "@tanstack/react-router";
import { SquareStack } from "lucide-react";
import type { ReactNode } from "react";

import { isStackGone, stackQuery } from "@/api/stacks";
import { isUnmanagedGone, managedStackIdOf, unmanagedStackQuery } from "@/api/unmanaged-stacks";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { RelativeTime } from "@/components/relative-time";
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
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { useEnvName } from "@/features/resources/use-env-name";
import { AutoDeployPausedAlert } from "@/features/stack-git/auto-deploy-paused-alert";
import { GitStrip } from "@/features/stack-git/git-strip";
import { GitSourceBadge } from "@/features/stack-git/sync-badges";
import { SyncErrorAlert } from "@/features/stack-git/sync-error-alert";
import {
  NotAdoptableNote,
  OrphanIdNote,
  PortainerAdvice,
  UnmanagedNote,
} from "@/features/stack-unmanaged/unmanaged-notes";
import { DeployBanner } from "@/features/stacks/deploy-banner";
import { DeployFailedAlert } from "@/features/stacks/deploy-failed-alert";
import { DraftBanner } from "@/features/stacks/draft-banner";
import { OperationLockAlert } from "@/features/stacks/operation-lock-alert";
import {
  PullAndRedeployAction,
  StackActions,
  StoppedAlertWithStart,
} from "@/features/stacks/stack-actions";
import { StackStateCell } from "@/features/stacks/stack-state-cell";
import { StackUpdateCell } from "@/features/stacks/stack-update-cell";
import { UnmanagedBadge } from "@/features/stacks/unmanaged-badge";
import { useDocumentTitle } from "@/hooks/use-me";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";

// The steps of a deploy, which the Review and the console routes show instead of the tab bar.
export type StackFlow = "review" | "deploy";

interface ManagedLayoutProps {
  // Absent or "managed" is the page of a stack TierPlan manages.
  variant?: "managed";
  envId: string;
  stackId: string;
  // The tab under the header. The route passes its Outlet; a test passes its own.
  children?: ReactNode;
  // Set on the Review and the console routes: the flow caption replaces the tab bar (UI-SPEC
  // "Review"). The header, the alerts and the breadcrumb stay.
  flow?: StackFlow;
  // True on the Editor tab, which is where the draft banner's Open editor would lead.
  editing?: boolean;
}

// The page of a stack TierPlan did not make (D-09): read-only, found by its label, named by its name and not an ID.
interface UnmanagedLayoutProps {
  variant: "unmanaged";
  envId: string;
  // The swarm namespace or the Compose project. The router built it, encoded.
  name: string;
  children?: ReactNode;
}

export type StackDetailLayoutProps = ManagedLayoutProps | UnmanagedLayoutProps;

// The tabs of the page, in the order they show. Each is a child route of the page under
// stacks_.$stackId.*; the Overview is the page's own address.
const TABS = [
  {
    id: "overview",
    to: "/env/$envId/stacks/$stackId",
    labelKey: "stacks.detail.tabs.overview",
    exact: true,
  },
  {
    id: "editor",
    to: "/env/$envId/stacks/$stackId/editor",
    labelKey: "stacks.detail.tabs.editor",
    exact: false,
  },
  {
    id: "history",
    to: "/env/$envId/stacks/$stackId/history",
    labelKey: "stackHistory.tab",
    exact: false,
  },
  {
    id: "resources",
    to: "/env/$envId/stacks/$stackId/resources",
    labelKey: "stackResources.tab",
    exact: false,
  },
  {
    id: "logs",
    to: "/env/$envId/stacks/$stackId/logs",
    labelKey: "stackLogs.tab",
    exact: false,
  },
] as const;

// The Git tab of a stack from a repository, last in the order. Other stacks have no such tab.
const GIT_TAB_ENTRY = {
  id: "git",
  to: "/env/$envId/stacks/$stackId/git",
  labelKey: "stackGit.tab.label",
  exact: false,
} as const;

interface TabEntry {
  id: string;
  to: (typeof TABS)[number]["to"] | typeof GIT_TAB_ENTRY.to;
  labelKey: string;
  exact: boolean;
}

// tabsOf is the tabs of a stack's page. A stack from a git repository reads Files where the others read Editor (the
// route is the same: its files are the repository's, read-only, and TierPlan's overlay files), and has the Git tab last.
function tabsOf(git: boolean): readonly TabEntry[] {
  if (!git) return TABS;
  return [
    ...TABS.map((tab) =>
      tab.id === "editor" ? { ...tab, labelKey: "stackEditor.tab.files" } : tab,
    ),
    GIT_TAB_ENTRY,
  ];
}

function Breadcrumb({ envId, name }: { envId: string; name: string | undefined }) {
  const t = useT();
  return (
    <nav aria-label={t("stacks.detail.breadcrumb.label")}>
      <ol className="flex flex-wrap items-center gap-x-2 text-xs text-muted-foreground [&>li:not(:last-child)]:after:ms-2 [&>li:not(:last-child)]:after:content-['/']">
        <li>
          <Link
            to="/env/$envId/stacks"
            params={{ envId }}
            className="rounded-sm text-accent-ink hover:underline"
          >
            {t("stacks.detail.breadcrumb.root")}
          </Link>
        </li>
        <li className="min-w-0">
          {name === undefined ? (
            <Skeleton className="h-3 w-24" />
          ) : (
            <span aria-current="page" className="break-all">
              {name}
            </span>
          )}
        </li>
      </ol>
    </nav>
  );
}

// DeployAction is the header's Deploy… (UI-SPEC "Stack page"): it opens the Review of the stack's
// saved draft against what is running, and never starts a deploy by itself. A user who may not
// deploy sees it off with the role it takes, and a user whose permissions are not known yet sees
// nothing rather than a button that may turn off.
function DeployAction({ envId, stackId }: { envId: string; stackId: string }) {
  const t = useT();
  const needs = useNeeds("stacks.deploy", envId);
  if (needs.allowed) {
    return (
      <Button asChild variant="outline" className="max-md:h-11">
        <Link to="/env/$envId/stacks/$stackId/review" params={{ envId, stackId }}>
          {t("stackReview.deploy.action")}
        </Link>
      </Button>
    );
  }
  if (!needs.reason) return null;
  return (
    <NeedsTooltip reason={needs.reason}>
      <Button type="button" variant="outline" className="max-md:h-11">
        {t("stackReview.deploy.action")}
      </Button>
    </NeedsTooltip>
  );
}

// AdoptAction is the unmanaged page's one action (UI-SPEC "Unmanaged stack page"): Adopt stack, which opens the
// Adopt page. An Env Admin gets a link; anyone else sees it off with the role it takes, and a stack that cannot be
// adopted (TierPlan's own, an agent's, a name its target cannot hold) with the server's reason. A user whose
// permissions are not known yet sees nothing rather than a button that may turn off. The muted line under it says
// what to do to remove the stack, since there is no Remove here (D-10).
function AdoptAction({
  envId,
  name,
  adoptable,
  reason,
}: {
  envId: string;
  name: string;
  adoptable: boolean;
  reason: string;
}) {
  const t = useT();
  const needs = useNeeds("stacks.create", envId);
  if (!needs.allowed && !needs.reason) return null;
  const blocked = needs.allowed ? (adoptable ? undefined : reason || undefined) : needs.reason;
  return (
    <div className="flex flex-col gap-1 max-md:w-full md:items-end">
      {needs.allowed && adoptable ? (
        <Button asChild className="max-md:h-11">
          <Link to="/env/$envId/unmanaged-stacks/$name/adopt" params={{ envId, name }}>
            {t("stackAdopt.action.adopt")}
          </Link>
        </Button>
      ) : (
        <NeedsTooltip reason={blocked}>
          <Button type="button" className="max-md:h-11" aria-disabled={blocked ? undefined : true}>
            {t("stackAdopt.action.adopt")}
          </Button>
        </NeedsTooltip>
      )}
      <p className="text-xs text-muted-foreground">{t("stackAdopt.action.hint")}</p>
    </div>
  );
}

// FlowCaption is the caption that replaces the tabs while a deploy is being made: "Step 2 of 3:
// Review" with the steps either side of it as plain text. The current step carries
// aria-current="step", and the words, not the weight, say where the person is.
function FlowCaption({ step }: { step: StackFlow }) {
  const t = useT();
  const steps = [
    { id: "edit", text: t("stackReview.flow.edit"), current: false },
    {
      id: "review",
      text: step === "review" ? t("stackReview.flow.reviewCurrent") : t("stackReview.flow.review"),
      current: step === "review",
    },
    {
      id: "deploy",
      text: step === "deploy" ? t("stackReview.flow.deployCurrent") : t("stackReview.flow.deploy"),
      current: step === "deploy",
    },
  ];
  return (
    <nav aria-label={t("stackReview.flow.label")}>
      <ol className="flex flex-wrap items-center gap-x-6 gap-y-1 text-sm text-muted-foreground">
        {steps.map((s) => (
          <li
            key={s.id}
            aria-current={s.current ? "step" : undefined}
            className={s.current ? "font-medium text-foreground" : undefined}
          >
            {s.text}
          </li>
        ))}
      </ol>
    </nav>
  );
}

// Gone is what a stack's page becomes when the stack no longer exists. It replaces the page,
// tabs included. An unmanaged stack's says it may be managed now and, when the server named the stack
// that holds the name, offers it.
function Gone({
  envId,
  unmanaged,
  managedStackId,
}: {
  envId: string;
  unmanaged?: boolean;
  managedStackId?: string;
}) {
  const t = useT();
  const ns = unmanaged ? "stackUnmanagedPage.gone" : "stacks.detail.gone";
  return (
    <div className="flex flex-col gap-6">
      <header>
        <Breadcrumb envId={envId} name={undefined} />
      </header>
      <Empty>
        <EmptyHeader>
          <EmptyMedia variant="icon">
            <SquareStack aria-hidden="true" />
          </EmptyMedia>
          <EmptyTitle role="heading" aria-level={2}>
            {t(`${ns}.heading`)}
          </EmptyTitle>
          <EmptyDescription>{t(`${ns}.body`)}</EmptyDescription>
        </EmptyHeader>
        <EmptyContent className="flex-row flex-wrap justify-center">
          <Button asChild variant="outline" className="max-md:h-11">
            <Link to="/env/$envId/stacks" params={{ envId }}>
              {t(`${ns}.back`)}
            </Link>
          </Button>
          {unmanaged && managedStackId ? (
            <Button asChild variant="outline" className="max-md:h-11">
              <Link to="/env/$envId/stacks/$stackId" params={{ envId, stackId: managedStackId }}>
                {t("stackUnmanagedPage.gone.open")}
              </Link>
            </Button>
          ) : null}
        </EmptyContent>
      </Empty>
    </div>
  );
}

// ManagedStackDetailLayout is the frame of a stack's page: the breadcrumb, the header (the name, its
// state, the target and source badges, and the line naming the environment and the last
// deploy), the tab bar (Link triggers, the active tab follows the URL) and the tab under it.
// The tabs render at once, before the stack has loaded (UI-SPEC "Loading"). A stack that is
// gone replaces the page.
function ManagedStackDetailLayout({ envId, stackId, children, flow, editing }: ManagedLayoutProps) {
  const t = useT();
  const envName = useEnvName(envId);
  const query = useQuery(stackQuery(envId, stackId));
  const { data } = query;
  const gone = isStackGone(query.error);
  useDocumentTitle(gone ? t("stacks.detail.gone.heading") : data?.name);

  if (gone) return <Gone envId={envId} />;

  if (query.isError && !data) {
    return (
      <div className="flex flex-col gap-6">
        <header>
          <Breadcrumb envId={envId} name={stackId} />
        </header>
        <Alert variant="destructive">
          <AlertDescription className="flex flex-wrap items-center gap-3">
            <span>{t("stacks.detail.error.loadFailed", { name: stackId })}</span>
            <span>{t("stacks.detail.error.body")}</span>
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="max-md:h-11"
              disabled={query.isFetching}
              onClick={() => void query.refetch()}
            >
              {t("stacks.detail.error.retry", { name: stackId })}
            </Button>
          </AlertDescription>
        </Alert>
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-6">
      <header className="flex flex-col gap-2">
        <Breadcrumb envId={envId} name={data?.name} />
        {data ? (
          <div className="flex flex-wrap items-start justify-between gap-x-4 gap-y-2">
            <div className="flex min-w-0 flex-col gap-1">
              <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
                <Tooltip>
                  <TooltipTrigger asChild>
                    <h2 className="line-clamp-2 max-w-full min-w-0 break-all text-2xl font-semibold">
                      {data.name}
                    </h2>
                  </TooltipTrigger>
                  <TooltipContent className="break-all">{data.name}</TooltipContent>
                </Tooltip>
                <StackStateCell
                  state={data.state}
                  running={data.servicesRunning}
                  total={data.servicesTotal}
                  failedService={data.lastOperation?.failedService}
                />
                {data.update ? <StackUpdateCell update={data.update} /> : null}
                <Badge variant="outline">{t(`stacks.target.${data.target}`)}</Badge>
                {data.git ? (
                  <GitSourceBadge git={data.git} />
                ) : (
                  <Badge variant="outline">{t(`stacks.source.${data.source}`)}</Badge>
                )}
                {data.hasDraft ? <Badge variant="outline">{t("stacks.draft")}</Badge> : null}
              </div>
              <p className="text-sm text-muted-foreground">
                {data.lastDeployedAt ? (
                  <>
                    {t("stacks.detail.meta.deployedPrefix", { env: envName })}{" "}
                    <RelativeTime value={data.lastDeployedAt} />{" "}
                    {t("stacks.list.lastDeployedBy", { user: data.lastDeployedBy })}
                  </>
                ) : (
                  t("stacks.detail.meta.neverDeployed", { env: envName })
                )}
              </p>
            </div>
            {flow ? null : (
              <div className="flex flex-wrap items-start gap-2 max-md:w-full">
                {data.git ? (
                  <PullAndRedeployAction stack={data} />
                ) : (
                  <DeployAction envId={envId} stackId={stackId} />
                )}
                <StackActions stack={data} />
              </div>
            )}
          </div>
        ) : (
          <Skeleton className="h-8 w-64" />
        )}
      </header>
      {/* The Git strip of a stack from a repository: under the header and above the alerts, on every tab. The Review
          and the console routes show the flow caption instead and do not repeat it. */}
      {data && !flow ? <GitStrip stack={data} /> : null}
      {/* The alerts, in the order of UI-SPEC "Stack page": an operation under way, the last deploy that
          stopped, another operation holding the environment, a stack that is stopped, a git stack that cannot sync, auto-deploy that is paused, a draft that waits.
          The console is the operation's own page, so it does not warn about itself, and a stack that
          is being deployed or removed is not also called stopped. */}
      {data && flow !== "deploy" ? <DeployBanner stack={data} /> : null}
      {data && flow !== "deploy" ? <DeployFailedAlert stack={data} /> : null}
      {data?.lock && flow !== "deploy" ? (
        <OperationLockAlert lock={data.lock} envId={envId} envName={envName} />
      ) : null}
      {data && flow !== "deploy" && data.lastOperation?.state !== "running" ? (
        <StoppedAlertWithStart stack={data} />
      ) : null}
      {data && !flow ? <SyncErrorAlert stack={data} /> : null}
      {data && !flow ? <AutoDeployPausedAlert stack={data} /> : null}
      {data && !flow && !editing ? <DraftBanner stack={data} envName={envName} /> : null}
      {flow ? (
        <FlowCaption step={flow} />
      ) : (
        <Tabs value="" className="gap-4">
          <TabsList
            variant="line"
            aria-label={t("stacks.detail.tabs.label")}
            className="h-9 max-md:h-11"
          >
            {tabsOf(data?.source === "git").map((tab) => (
              <TabsTrigger
                key={tab.id}
                value={tab.id}
                asChild
                className="data-active:text-accent-ink after:bg-accent-ink max-md:h-11"
              >
                <Link
                  to={tab.to}
                  params={{ envId, stackId }}
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
      )}
      {children ?? <Outlet />}
    </div>
  );
}

// The tabs of an unmanaged stack's page: no Editor and no History, since TierPlan holds no definition of it. Each
// is a child route of the page under unmanaged-stacks.$name.*.
const UNMANAGED_TABS = [
  {
    id: "overview",
    to: "/env/$envId/unmanaged-stacks/$name",
    labelKey: "stacks.detail.tabs.overview",
    exact: true,
  },
  {
    id: "compose",
    to: "/env/$envId/unmanaged-stacks/$name/compose",
    labelKey: "stackGenerated.tab",
    exact: false,
  },
  {
    id: "resources",
    to: "/env/$envId/unmanaged-stacks/$name/resources",
    labelKey: "stackResources.tab",
    exact: false,
  },
  {
    id: "logs",
    to: "/env/$envId/unmanaged-stacks/$name/logs",
    labelKey: "stackLogs.tab",
    exact: false,
  },
] as const;

// UnmanagedDetailLayout is the frame of an unmanaged stack's page (UI-SPEC "Unmanaged stack page"): the
// breadcrumb, the header (the name, its state, the target, Unmanaged, Portainer and owner badges, and the line
// saying TierPlan does not manage it), the notes, the tab bar and the tab under it. Its one action is Adopt stack, which
// opens the Adopt page and changes nothing by itself (D-10, D-14). A stack that is gone, or managed now, replaces the
// page.
function UnmanagedDetailLayout({ envId, name, children }: Omit<UnmanagedLayoutProps, "variant">) {
  const t = useT();
  const envName = useEnvName(envId);
  const query = useQuery(unmanagedStackQuery(envId, name));
  const { data } = query;
  const gone = isUnmanagedGone(query.error);
  useDocumentTitle(gone ? t("stackUnmanagedPage.gone.heading") : name);

  if (gone) {
    return <Gone envId={envId} unmanaged managedStackId={managedStackIdOf(query.error)} />;
  }

  if (query.isError && !data) {
    return (
      <div className="flex flex-col gap-6">
        <header>
          <Breadcrumb envId={envId} name={name} />
        </header>
        <Alert variant="destructive">
          <AlertDescription className="flex flex-wrap items-center gap-3">
            <span>{t("stackUnmanagedPage.error.loadFailed", { name })}</span>
            <span>{t("stackUnmanagedPage.error.body")}</span>
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="max-md:h-11"
              disabled={query.isFetching}
              onClick={() => void query.refetch()}
            >
              {t("stackUnmanagedPage.error.retry", { name })}
            </Button>
          </AlertDescription>
        </Alert>
      </div>
    );
  }

  const owner = data?.owner;
  return (
    <div className="flex flex-col gap-6">
      <header className="flex flex-col gap-2">
        <Breadcrumb envId={envId} name={name} />
        {data ? (
          <div className="flex flex-wrap items-start justify-between gap-x-4 gap-y-2">
            <div className="flex min-w-0 flex-col gap-1">
              <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
                <Tooltip>
                  <TooltipTrigger asChild>
                    <h2 className="line-clamp-2 max-w-full min-w-0 break-all text-2xl font-semibold">
                      {name}
                    </h2>
                  </TooltipTrigger>
                  <TooltipContent className="break-all">{name}</TooltipContent>
                </Tooltip>
                <StackStateCell
                  state={data.state}
                  running={data.servicesRunning}
                  total={data.servicesTotal}
                />
                {data.update ? <StackUpdateCell update={data.update} /> : null}
                <Badge variant="outline">{t(`stacks.target.${data.target}`)}</Badge>
                <UnmanagedBadge orphan={data.orphan} />
                {data.portainer ? (
                  <Badge variant="outline">{t("stackUnmanaged.portainer")}</Badge>
                ) : null}
                {owner === "tierplan" || owner === "agent" ? (
                  <Badge variant="outline">{t(`stackUnmanagedPage.owner.${owner}`)}</Badge>
                ) : null}
              </div>
              <p className="text-sm text-muted-foreground">
                {t("stackUnmanagedPage.meta", { env: envName })}
              </p>
            </div>
            <AdoptAction
              envId={envId}
              name={name}
              adoptable={data.adoptable}
              reason={data.reason}
            />
          </div>
        ) : (
          <Skeleton className="h-8 w-64" />
        )}
      </header>
      {data ? (
        <>
          <UnmanagedNote />
          {!data.adoptable && data.reason ? <NotAdoptableNote reason={data.reason} /> : null}
          {data.orphan ? <OrphanIdNote /> : null}
          {data.portainer ? <PortainerAdvice /> : null}
        </>
      ) : (
        <Skeleton className="h-16 w-full" aria-hidden="true" />
      )}
      <Tabs value="" className="gap-4">
        <TabsList
          variant="line"
          aria-label={t("stacks.detail.tabs.label")}
          className="h-9 max-md:h-11"
        >
          {UNMANAGED_TABS.map((tab) => (
            <TabsTrigger
              key={tab.id}
              value={tab.id}
              asChild
              className="data-active:text-accent-ink after:bg-accent-ink max-md:h-11"
            >
              <Link
                to={tab.to}
                params={{ envId, name }}
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
      {children ?? <Outlet />}
    </div>
  );
}

// StackDetailLayout is the frame of a stack's page, for a stack TierPlan manages (the default) and for one it
// found on the engine and does not manage (variant "unmanaged"). The two are separate components so that each
// reads only its own stack.
export function StackDetailLayout(props: StackDetailLayoutProps) {
  if (props.variant === "unmanaged") {
    return (
      <UnmanagedDetailLayout envId={props.envId} name={props.name}>
        {props.children}
      </UnmanagedDetailLayout>
    );
  }
  return <ManagedStackDetailLayout {...props} />;
}
