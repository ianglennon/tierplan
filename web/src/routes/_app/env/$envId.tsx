// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { createFileRoute, Link, Outlet } from "@tanstack/react-router";
import {
  FileQuestion,
  Network,
  RefreshCw,
  ServerCrash,
  ServerOff,
  ShieldOff,
  Unplug,
} from "lucide-react";
import { type ReactNode, useEffect, useRef, useState } from "react";

import {
  type EnvironmentItem,
  EnvironmentProblemError,
  environmentsQuery,
  isProblemState,
  type ProblemState,
  recentQuery,
  reconnectEnvironment,
  recordRecent,
  summaryQuery,
} from "@/api/environments";
import { FullPageState, NoPermission } from "@/components/full-page-state";
import { useNow } from "@/components/relative-time";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { EnvEventsProvider } from "@/features/dashboard/events";
import { AdoptClusterDialog } from "@/features/environments/adopt-cluster-dialog";
import { EnvDegradedBanner } from "@/features/environments/degraded-banner";
import { InstallDialog } from "@/features/environments/install-dialog";
import { isKnownReason, ReasonText } from "@/features/environments/reason-text";
import { useAccessRefetching } from "@/hooks/use-access-refetch";
import { useDocumentTitle, usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { formatClock, formatRelative } from "@/lib/format";

// How Retry waits for the answer. The reconnect request only asks the health
// monitor to probe now (202), so the summary is read again a few times until
// the engine answers or the tries run out. Exported so tests can shorten it.
export const retryTiming = { polls: 4, delayMs: 600 };

// How long an environment must stay open before it is recorded as last used.
// Exported so tests can shorten it.
export const recordRecentTiming = { delayMs: 2_000 };

const sleep = (ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms));

// PageTitle sets the tab title while a full-page state is showing. It is a
// component, not a hook call in the layout, because the layout's effect would
// run after the page's own and overwrite its title.
function PageTitle({ page }: { page: string }) {
  useDocumentTitle(page);
  return null;
}

// Attempted is the "Attempted {relative time}" line under a full-page state
// after Retry ran and the problem is still there, so the user can see it ran.
function Attempted({ at }: { at: number }) {
  const t = useT();
  const now = useNow();
  const when = formatRelative(new Date(at).toISOString(), new Date(now), t);
  return <>{t("dashboard.fullPage.attempted", { when })}</>;
}

// useRecordRecent tells the server this environment was opened, once it has
// stayed open for recordRecentTiming.delayMs (D-31). Passing through it, or the
// layout rendering again, does not count: the timer belongs to the environment,
// not to a render, and leaving first cancels it. A failure is ignored, since
// the person who opened the environment cannot do anything about it.
function useRecordRecent(envId: string): void {
  const queryClient = useQueryClient();
  useEffect(() => {
    const timer = setTimeout(() => {
      void recordRecent(envId).then((ok) => {
        if (ok) void queryClient.invalidateQueries({ queryKey: recentQuery.queryKey });
      });
    }, recordRecentTiming.delayMs);
    return () => clearTimeout(timer);
  }, [envId, queryClient]);
}

// The codes only the local Docker socket reports. With them, or with none, an
// offline environment is the Phase 1 "Docker isn't reachable" page.
const LOCAL_REASONS: ReadonlySet<string> = new Set([
  "unreachable",
  "api_too_old",
  "podman",
  "rootless",
  "docker_desktop",
]);

// The states only an environment whose hosts dial in through an agent can be in.
const AGENT_ONLY: ReadonlySet<string> = new Set([
  "pending",
  "reconnecting",
  "revoked",
  "cluster_changed",
]);

// isAgentEnvironment says whether the offline wording is the agent's ("Coaming
// lost contact with the agent") or the local socket's. The list knows; until it
// has answered, the reason code tells.
function isAgentEnvironment(item: EnvironmentItem | undefined, problem: EnvironmentProblemError) {
  if (item) return item.kind === "agent";
  if (problem.state && AGENT_ONLY.has(problem.state)) return true;
  return problem.reason !== undefined && !LOCAL_REASONS.has(problem.reason);
}

// The destructive outline of a button that removes something (UI-SPEC).
const DESTRUCTIVE_OUTLINE =
  "border-destructive/40 text-status-offline-text hover:text-status-offline-text";

interface StateProps {
  envId: string;
  name: string;
}

// PendingPage is an environment no agent has enrolled into yet. Someone who may
// mint a token gets the install command (the dialog's rotate mode asks before it
// rotates); anyone else is told whom to ask.
function PendingPage({ envId, name }: StateProps) {
  const t = useT();
  const canToken = usePermission("environments.token");
  const [open, setOpen] = useState(false);
  const trigger = useRef<HTMLButtonElement>(null);
  const heading = t("fullpage.pending.heading");
  return (
    <>
      <PageTitle page={heading} />
      <FullPageState
        icon={Unplug}
        heading={heading}
        body={t(canToken ? "fullpage.pending.body" : "fullpage.pending.bodyNoToken", { name })}
        action={
          canToken ? (
            <Button ref={trigger} onClick={() => setOpen(true)}>
              {t("fullpage.pending.action")}
            </Button>
          ) : undefined
        }
      />
      {canToken ? (
        <InstallDialog
          open={open}
          onOpenChange={setOpen}
          mode="rotate"
          envId={envId}
          returnFocusRef={trigger}
        />
      ) : null}
    </>
  );
}

interface ProblemLinks {
  envId: string;
  // Whether this state offers the node list at all (a revoked environment does).
  settings: boolean;
  canConfigure: boolean;
  canDelete: boolean;
  t: ReturnType<typeof useT>;
}

// problemActions are the links of a revoked or re-initialised environment: the
// node list in its settings and, for someone who may delete it, the Remove card
// there. The Remove card is /settings/environments/$envId#remove (plan 29 builds
// it; until then the link lands on the page). Undefined when the caller may do
// neither, so the page has no empty row of actions.
function problemActions({ envId, settings, canConfigure, canDelete, t }: ProblemLinks) {
  const links: ReactNode[] = [];
  if (settings && canConfigure) {
    links.push(
      <Button key="settings" asChild variant="secondary">
        <Link to="/settings/environments/$envId" params={{ envId }}>
          {t("fullpage.revoked.openSettings")}
        </Link>
      </Button>,
    );
  }
  if (canDelete) {
    links.push(
      <Button key="remove" asChild variant="outline" className={DESTRUCTIVE_OUTLINE}>
        <Link to="/settings/environments/$envId" params={{ envId }} hash="remove">
          {t("fullpage.revoked.remove")}
        </Link>
      </Button>,
    );
  }
  return links.length > 0 ? links : undefined;
}

interface ClusterChangedProps extends StateProps {
  // The environment as the list knows it, for the cluster ID the dialog names.
  item: EnvironmentItem | undefined;
  canConfigure: boolean;
  canDelete: boolean;
}

// ClusterChangedPage is a swarm that was re-initialised: its agents report a
// different cluster, so Coaming refused them and changed nothing. Someone who may
// manage the environment can adopt the new cluster (the primary action); someone
// who may delete it can remove it instead (D-14).
function ClusterChangedPage({ envId, name, item, canConfigure, canDelete }: ClusterChangedProps) {
  const t = useT();
  const [open, setOpen] = useState(false);
  const trigger = useRef<HTMLButtonElement>(null);
  const heading = t("fullpage.clusterChanged.heading");
  const links = problemActions({ envId, settings: false, canConfigure, canDelete, t }) ?? [];
  return (
    <>
      <PageTitle page={heading} />
      <FullPageState
        icon={Network}
        tone="warning"
        heading={heading}
        body={t("fullpage.clusterChanged.body", { name })}
        action={
          canConfigure || links.length > 0 ? (
            <>
              {canConfigure ? (
                <Button ref={trigger} type="button" onClick={() => setOpen(true)}>
                  {t("fullpage.clusterChanged.adopt")}
                </Button>
              ) : null}
              {links}
            </>
          ) : undefined
        }
      />
      {canConfigure ? (
        <AdoptClusterDialog
          environment={{ id: envId, name, pendingClusterId: item?.pendingClusterId }}
          open={open}
          onOpenChange={setOpen}
          returnFocusRef={trigger}
        />
      ) : null}
    </>
  );
}

// EnvironmentLayout wraps every page of one environment. It reads the summary
// and turns the server's problem answers into full-page states: 503
// environment_offline (pending, reconnecting, offline, revoked or
// cluster_changed, told apart by the problem's state), 409
// environment_unsupported, 404 environment_not_found and 403. Anything else
// renders the page, which shows its own inline error. While one is showing, the
// environments cache the health stream keeps current is watched, and the summary
// is read again when it disagrees, so the page comes back by itself.
function EnvironmentLayout() {
  const { envId } = Route.useParams();
  const t = useT();
  const queryClient = useQueryClient();
  const summary = useQuery(summaryQuery(envId));
  const environments = useQuery(environmentsQuery);
  useRecordRecent(envId);
  const accessRefetching = useAccessRefetching();
  const [attempted, setAttempted] = useState<{ envId: string; at: number } | undefined>();

  const item = environments.data?.items?.find((e) => e.id === envId);
  const name = item?.name ?? t("dashboard.environmentFallback");
  // Adopting a changed cluster and opening the environment's settings need
  // environments.configure on it (D-07).
  const canConfigure = usePermission("environments.configure", envId);
  const canDelete = usePermission("environments.delete");

  const retry = useMutation({
    mutationFn: async () => {
      await reconnectEnvironment(envId);
      for (let i = 0; i < retryTiming.polls; i++) {
        await sleep(retryTiming.delayMs);
        const result = await summary.refetch();
        if (!result.isError) return;
      }
    },
    onSettled: () => {
      setAttempted({ envId, at: Date.now() });
      // The switcher's dot follows what the monitor now believes.
      void queryClient.invalidateQueries({ queryKey: environmentsQuery.queryKey });
    },
  });

  const problem = summary.error instanceof EnvironmentProblemError ? summary.error : undefined;
  const failure = problem?.failure;
  const engine = problem?.engine;
  const podman = engine?.name?.toLowerCase().includes("podman") || problem?.reason === "podman";

  // The state the layout is showing a full page for, if any.
  const shown: ProblemState | undefined =
    failure === "offline" || failure === "unsupported"
      ? (problem?.state ?? (failure === "unsupported" ? "unsupported" : "offline"))
      : undefined;
  // What the health stream says, through the environments cache.
  const live = item?.status.state;
  const settled = summary.isSuccess || summary.isError;
  const { refetch } = summary;
  // The page follows the stream (D-33): when the stream's state is not the one
  // this page was built for, read the summary again. That is how a page goes
  // back to the dashboard once the agent returns, from Reconnecting to Offline
  // when the grace ends, and from the dashboard to a full page when the
  // environment drops. It runs once per change, so a server that still
  // disagrees afterwards cannot make it loop.
  useEffect(() => {
    if (!settled || !live || live === "unknown") return;
    if (failure === "forbidden" || failure === "not_found") return;
    const stale = shown ? live !== shown : isProblemState(live);
    if (stale) void refetch();
  }, [live, shown, settled, failure, refetch]);

  // The server just refused this environment and the app is reading who the user
  // is now (D-10). Until it knows, the content is a skeleton: neither "Page not
  // found" nor the no-access state may flash before the move.
  if ((failure === "forbidden" || failure === "not_found") && accessRefetching) {
    return (
      <output
        aria-label={t("noaccess.checking")}
        className="mx-auto flex w-full max-w-7xl flex-col gap-4"
      >
        <Skeleton className="h-8 w-48" aria-hidden="true" />
        <Skeleton className="h-48 w-full" aria-hidden="true" />
      </output>
    );
  }

  // A failing environment never breaks the shell or other routes (D-26): only
  // this environment's own pages are replaced by a full-page state.
  if (failure === "forbidden") {
    return (
      <>
        <PageTitle page={t("common.noPermission.heading")} />
        <NoPermission />
      </>
    );
  }
  if (failure === "not_found") {
    return (
      <>
        <PageTitle page={t("common.notFound.heading")} />
        <FullPageState
          icon={FileQuestion}
          heading={t("common.notFound.heading")}
          body={t("common.notFound.body")}
          action={
            <Button asChild>
              <Link to="/">{t("common.notFound.action")}</Link>
            </Button>
          }
        />
      </>
    );
  }
  if (shown === "pending") return <PendingPage envId={envId} name={name} />;
  if (shown === "reconnecting") {
    const heading = t("fullpage.reconnecting.heading", { name });
    return (
      <>
        <PageTitle page={heading} />
        <FullPageState
          icon={RefreshCw}
          tone="warning"
          pulse
          heading={heading}
          body={t("fullpage.reconnecting.body")}
        />
      </>
    );
  }
  if (shown === "revoked") {
    const heading = t("fullpage.revoked.heading");
    return (
      <>
        <PageTitle page={heading} />
        <FullPageState
          icon={ShieldOff}
          tone="offline"
          heading={heading}
          body={t("fullpage.revoked.body", { name })}
          action={problemActions({ envId, settings: true, canConfigure, canDelete, t })}
        />
      </>
    );
  }
  if (shown === "cluster_changed") {
    return (
      <ClusterChangedPage
        envId={envId}
        name={name}
        item={item}
        canConfigure={canConfigure}
        canDelete={canDelete}
      />
    );
  }
  if (failure === "offline" || failure === "unsupported") {
    const lastTry = attempted?.envId === envId ? attempted.at : undefined;
    const action = (
      <Button onClick={() => retry.mutate()} disabled={retry.isPending}>
        {retry.isPending ? <Spinner aria-hidden="true" /> : null}
        {retry.isPending ? t("dashboard.fullPage.retrying") : t("dashboard.fullPage.retry")}
      </Button>
    );
    const footnote = lastTry === undefined ? undefined : <Attempted at={lastTry} />;

    if (failure === "offline" && problem && isAgentEnvironment(item, problem)) {
      const heading = t("fullpage.offline.heading", { name });
      const time = formatClock(problem.since);
      // An agent's refusal is worded by ReasonText; with no code the agent
      // simply isn't connected. The type picks which check command fits.
      const code = isKnownReason(problem.reason) ? problem.reason : "agent_offline";
      return (
        <>
          <PageTitle page={heading} />
          <FullPageState
            icon={ServerOff}
            tone="offline"
            heading={heading}
            body={
              time
                ? t("fullpage.offline.body", { name, time })
                : t("fullpage.offline.bodyNoTime", { name })
            }
            detail={<ReasonText code={code} params={{ envType: item?.envType }} />}
            action={action}
            footnote={footnote}
          />
        </>
      );
    }
    if (failure === "offline") {
      const heading = t("dashboard.fullPage.offline.heading");
      const time = formatClock(problem?.since);
      return (
        <>
          <PageTitle page={heading} />
          <FullPageState
            icon={ServerOff}
            heading={heading}
            body={
              time
                ? t("dashboard.fullPage.offline.body", { name, time })
                : t("dashboard.fullPage.offline.bodyNoTime", { name })
            }
            action={action}
            footnote={footnote}
          />
        </>
      );
    }

    const heading = podman
      ? t("dashboard.fullPage.unsupported.podmanHeading")
      : t("dashboard.fullPage.unsupported.heading");
    const facts = {
      engine: engine?.name ?? t("dashboard.fullPage.unsupported.engineFallback"),
      version: engine?.version ?? "—",
      api: engine?.apiVersion ?? "—",
    };
    // Only a too-old API has the "below the minimum" wording. Rootless and
    // Docker Desktop engines are unsupported for other reasons.
    const tooOld = problem?.reason === undefined || problem.reason === "api_too_old";
    return (
      <>
        <PageTitle page={heading} />
        <FullPageState
          icon={ServerCrash}
          heading={heading}
          body={
            podman
              ? t("dashboard.fullPage.unsupported.podmanBody")
              : tooOld
                ? t("dashboard.fullPage.unsupported.body", facts)
                : t("dashboard.fullPage.unsupported.bodyOther", facts)
          }
          action={action}
          footnote={footnote}
        />
      </>
    );
  }
  // Only an environment with none of those problems gets here. The events subscription opens once the summary has answered,
  // so none is opened for an environment that cannot, and it sits around the
  // whole outlet so the dashboard's feed and every list view share it (D-57).
  // A degraded environment still works, so its pages show, with the banner at
  // the top of every one of them. It follows the health stream, so it goes away
  // by itself when the environment recovers.
  return (
    <>
      {item?.status.state === "degraded" ? (
        <div className="mx-auto mb-6 w-full max-w-7xl">
          <EnvDegradedBanner environment={item} />
        </div>
      ) : null}
      <EnvEventsProvider envId={envId} enabled={summary.data !== undefined}>
        <Outlet />
      </EnvEventsProvider>
    </>
  );
}

export const Route = createFileRoute("/_app/env/$envId")({
  component: EnvironmentLayout,
});
