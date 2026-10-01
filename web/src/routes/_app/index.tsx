// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { createFileRoute } from "@tanstack/react-router";
import { Server } from "lucide-react";

import { api } from "@/api/client";
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

// The ["environments"] query: every environment the caller may read. The panel
// invalidates it after adding the local host.
const environmentsQuery = {
  queryKey: ["environments"],
  queryFn: async () => {
    const { data } = await api.GET("/api/environments");
    if (!data) throw new Error("GET /api/environments failed");
    return data;
  },
};

// The signed-in landing page. With no environment there is nothing to show but
// where to start (D-08): the opt-in for this host, or the command that mounts
// the Docker socket. Plan 19 sends users to their first environment when there
// is one.
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
  if ((environments.data.items ?? []).length > 0) return null;

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
  component: Home,
});
