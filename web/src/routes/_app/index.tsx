// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { createFileRoute, Navigate, redirect } from "@tanstack/react-router";
import { Server } from "lucide-react";

import { environmentsQuery } from "@/api/environments";
import { queryClient } from "@/api/query-client";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Card, CardContent } from "@/components/ui/card";
import {
  Empty,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@/components/ui/empty";
import { LocalHostPanel } from "@/features/environments/local-host-panel";
import { useDocumentTitle, usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

// The signed-in landing page. With no environment there is nothing to show but
// where to start (D-08): the opt-in for this host, or the command that mounts
// the Docker socket. With one, the user goes to its dashboard: the route's
// beforeLoad does it on arrival, and the Navigate below does it when the list
// fills while this page is open (the opt-in just added the host).
function Home() {
  const t = useT();
  useDocumentTitle(t("shell.home.tabTitle"));
  const canAdd = usePermission("environments.write");
  const environments = useQuery(environmentsQuery);

  if (environments.isError) {
    return (
      <Alert variant="destructive">
        <AlertDescription>{t("common.errors.unreachable")}</AlertDescription>
      </Alert>
    );
  }
  // Nothing until the list has answered, so the empty state never flashes up
  // for someone who has environments.
  if (!environments.data) return null;
  const first = (environments.data.items ?? [])[0];
  if (first) return <Navigate to="/env/$envId" params={{ envId: first.id }} replace />;

  return (
    <div className="mx-auto flex w-full max-w-120 flex-col gap-6">
      <Empty>
        <EmptyHeader>
          <EmptyMedia variant="icon">
            <Server aria-hidden="true" />
          </EmptyMedia>
          <EmptyTitle>{t("env.empty.heading")}</EmptyTitle>
          <EmptyDescription>{t("env.empty.body")}</EmptyDescription>
        </EmptyHeader>
      </Empty>
      {canAdd ? (
        <Card className="py-6">
          <CardContent className="px-6">
            <LocalHostPanel mode="settings" />
          </CardContent>
        </Card>
      ) : (
        <p className="text-center text-sm text-muted-foreground">{t("env.empty.noPermission")}</p>
      )}
    </div>
  );
}

export const Route = createFileRoute("/_app/")({
  // A signed-in user with an environment lands on its dashboard (ENV-07). A
  // list that cannot be read falls through to the page, which says so.
  beforeLoad: async () => {
    let first: string | undefined;
    try {
      // A short staleTime: the list is read again when it is older than that,
      // so a host that was removed does not send the user back to a missing
      // dashboard.
      const list = await queryClient.fetchQuery({
        ...environmentsQuery,
        staleTime: 5_000,
        retry: false,
      });
      first = list.items?.[0]?.id;
    } catch {
      return;
    }
    if (first) throw redirect({ to: "/env/$envId", params: { envId: first } });
  },
  component: Home,
});
