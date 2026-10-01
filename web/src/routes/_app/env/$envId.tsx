// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { createFileRoute, Link, Outlet } from "@tanstack/react-router";
import { FileQuestion, ServerCrash, ServerOff } from "lucide-react";
import { useEffect, useState } from "react";

import {
  EnvironmentProblemError,
  environmentsQuery,
  reconnectEnvironment,
  summaryQuery,
} from "@/api/environments";
import { FullPageState, NoPermission } from "@/components/full-page-state";
import { Button } from "@/components/ui/button";
import { Spinner } from "@/components/ui/spinner";
import { useDocumentTitle } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { formatClock } from "@/lib/clock";

// How Retry waits for the answer. The reconnect request only asks the health
// monitor to probe now (202), so the summary is read again a few times until
// the engine answers or the tries run out. Exported so tests can shorten it.
export const retryTiming = { polls: 4, delayMs: 600 };

const sleep = (ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms));

// useNow ticks every 30 seconds so "Attempted 2 min ago" stays true.
function useNow(): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), 30_000);
    return () => clearInterval(id);
  }, []);
  return now;
}

// Attempted is the "Attempted {relative time}" line under a full-page state
// after Retry ran and the problem is still there, so the user can see it ran.
function Attempted({ at }: { at: number }) {
  const t = useT();
  const now = useNow();
  const minutes = Math.max(0, Math.floor((now - at) / 60_000));
  const when =
    minutes < 1
      ? t("dashboard.fullPage.time.justNow")
      : minutes < 60
        ? t("dashboard.fullPage.time.minutesAgo", { count: minutes })
        : t("dashboard.fullPage.time.hoursAgo", { count: Math.floor(minutes / 60) });
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

  let heading: string | undefined;
  if (failure === "offline") heading = t("dashboard.fullPage.offline.heading");
  if (failure === "unsupported") {
    heading =
      problem?.engine?.name?.toLowerCase().includes("podman") || problem?.reason === "podman"
        ? t("dashboard.fullPage.unsupported.podmanHeading")
        : t("dashboard.fullPage.unsupported.heading");
  }
  if (failure === "not_found") heading = t("common.notFound.heading");
  useDocumentTitle(
    heading ?? (failure === "forbidden" ? t("common.noPermission.heading") : undefined),
  );

  // A failing environment never breaks the shell or other routes (D-26): only
  // this environment's own pages are replaced by a full-page state.
  if (failure === "forbidden") return <NoPermission />;
  if (failure === "not_found") {
    return (
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
      const time = formatClock(problem?.since);
      return (
        <FullPageState
          icon={ServerOff}
          heading={t("dashboard.fullPage.offline.heading")}
          body={
            time
              ? t("dashboard.fullPage.offline.body", { name, time })
              : t("dashboard.fullPage.offline.bodyNoTime", { name })
          }
          action={action}
          footnote={footnote}
        />
      );
    }

    const engine = problem?.engine;
    const podman = engine?.name?.toLowerCase().includes("podman") || problem?.reason === "podman";
    const facts = {
      engine: engine?.name ?? t("dashboard.fullPage.unsupported.engineFallback"),
      version: engine?.version ?? "—",
      api: engine?.apiVersion ?? "—",
    };
    const tooOld = problem?.reason === undefined || problem.reason === "api_too_old";
    return (
      <FullPageState
        icon={ServerCrash}
        heading={heading ?? t("dashboard.fullPage.unsupported.heading")}
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
    );
  }
  return <Outlet />;
}

export const Route = createFileRoute("/_app/env/$envId")({
  component: EnvironmentLayout,
});
