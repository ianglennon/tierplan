// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { createFileRoute } from "@tanstack/react-router";
import { Server } from "lucide-react";

import {
  Empty,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@/components/ui/empty";
import { useDocumentTitle } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

// The signed-in landing page. Until a host is managed there is nothing to show
// but where to start (D-08). Plan 17 adds the copyable docker run snippet and the
// "Manage this host" opt-in, and plan 19 sends users to their first environment.
function Home() {
  const t = useT();
  useDocumentTitle(t("shell.home.tabTitle"));
  return (
    <Empty>
      <EmptyHeader>
        <EmptyMedia variant="icon">
          <Server aria-hidden="true" />
        </EmptyMedia>
        <EmptyTitle>{t("shell.home.noEnvironments.heading")}</EmptyTitle>
        <EmptyDescription>{t("shell.home.noEnvironments.body")}</EmptyDescription>
      </EmptyHeader>
    </Empty>
  );
}

export const Route = createFileRoute("/_app/")({
  component: Home,
});
