// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { createFileRoute, Link, Outlet } from "@tanstack/react-router";
import { FileQuestion, ServerCrash, ServerOff } from "lucide-react";
import { useState } from "react";

import {
  EnvironmentProblemError,
  environmentsQuery,
  reconnectEnvironment,
  summaryQuery,
} from "@/api/environments";
import { FullPageState, NoPermission } from "@/components/full-page-state";
import { useNow } from "@/components/relative-time";
import { Button } from "@/components/ui/button";
import { Spinner } from "@/components/ui/spinner";
import { EnvEventsProvider } from "@/features/dashboard/events";
import { useDocumentTitle } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { formatClock, formatRelative } from "@/lib/format";

// How Retry waits for the answer. The reconnect request only asks the health
// monitor to probe now (202), so the summary is read again a few times until
// the engine answers or the tries run out. Exported so tests can shorten it.
export const retryTiming = { polls: 4, delayMs: 600 };

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

// EnvironmentLayout wraps every page of one environment. It reads the summary
// and turns the server's problem answers into full-page states: 503
// environment_offline, 409 environment_unsupported, 404 environment_not_found
// and 403. Anything else renders the page, which shows its own inline error.
function EnvironmentLayout() {
  const { envId } = Route.useParams();
  const t = useT();
  const queryClient = useQueryClient();
  const summary = useQuery(summaryQuery(envId));
  const environments = useQuery(environmentsQuery);
  const [attempted, setAttempted] = useState<{ envId: string; at: number } | undefined>();

  const name =
    environments.data?.items?.find((e) => e.id === envId)?.name ??
    t("dashboard.environmentFallback");

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
  if (failure === "offline" || failure === "unsupported") {
    const lastTry = attempted?.envId === envId ? attempted.at : undefined;
    const action = (
      <Button onClick={() => retry.mutate()} disabled={retry.isPending}>
        {retry.isPending ? <Spinner aria-hidden="true" /> : null}
        {retry.isPending ? t("dashboard.fullPage.retrying") : t("dashboard.fullPage.retry")}
      </Button>
    );
    const footnote = lastTry === undefined ? undefined : <Attempted at={lastTry} />;

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
  // Only an environment that is not offline, unsupported, missing or forbidden
  // gets here. The events subscription opens once the summary has answered,
  // so none is opened for an environment that cannot, and it sits around the
  // whole outlet so the dashboard's feed and every list view share it (D-57).
  return (
    <EnvEventsProvider envId={envId} enabled={summary.data !== undefined}>
      <Outlet />
    </EnvEventsProvider>
  );
}

export const Route = createFileRoute("/_app/env/$envId")({
  component: EnvironmentLayout,
});
