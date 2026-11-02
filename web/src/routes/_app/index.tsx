// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import { createFileRoute, Navigate } from "@tanstack/react-router";

import { environmentsQuery, recentQuery } from "@/api/environments";
import { NoAccessState } from "@/components/no-access-state";
import { Skeleton } from "@/components/ui/skeleton";
import { hasNoEnvironmentAccess, useMe } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

// A short staleTime: both lists are read again when they are older than this,
// so an environment that was removed does not send the user to a missing
// dashboard, and the one just opened (which invalidates the recent list) comes
// first. Neither read is retried: the overview is the fallback and says what
// went wrong.
const FRESH_MS = 5_000;

// Home is the signed-in landing page (D-31): the environment this user last
// opened, on any device, else the overview. The server keeps the last three per
// user and leaves out what was removed or can no longer be read; the list is
// checked as well, so the first recent one still in it wins. Any failure falls
// to /environments, which shows its own error with Retry.
//
// It is a page and not a beforeLoad redirect so there is something to show
// while the answer loads: the router renders nothing for a route whose
// beforeLoad is pending. That something is a skeleton, never the empty state,
// which lives on /environments.
function Home() {
  const t = useT();
  const { data: me } = useMe();
  // A user with no access to any environment has nothing to open: they get the
  // no-access state here, in the shell, and the lists are not read (D-08).
  const noAccess = hasNoEnvironmentAccess(me);
  const recent = useQuery({
    ...recentQuery,
    staleTime: FRESH_MS,
    retry: false,
    enabled: !noAccess,
  });
  const list = useQuery({
    ...environmentsQuery,
    staleTime: FRESH_MS,
    retry: false,
    enabled: !noAccess,
  });

  if (noAccess) return <NoAccessState administrators={me?.administrators} />;

  const settled = [recent, list].every((q) => !q.isPending && !q.isFetching);
  if (settled) {
    const readable = new Set((list.data?.items ?? []).map((e) => e.id));
    const target = (recent.data ?? []).find((id) => readable.has(id));
    if (target) return <Navigate to="/env/$envId" params={{ envId: target }} replace />;
    return <Navigate to="/environments" replace />;
  }

  return (
    <output
      aria-label={t("overview.loading")}
      className="mx-auto flex w-full max-w-7xl flex-col gap-4"
    >
      <Skeleton className="h-8 w-48" aria-hidden="true" />
      <Skeleton className="h-48 w-full" aria-hidden="true" />
    </output>
  );
}

export const Route = createFileRoute("/_app/")({
  component: Home,
});
