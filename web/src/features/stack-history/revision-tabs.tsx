// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { useState } from "react";

import { revisionQuery } from "@/api/stack-history";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { RevisionContents } from "@/features/stack-history/revision-contents";
import { RevisionRendered } from "@/features/stack-history/revision-rendered";
import { RevisionYaml } from "@/features/stack-history/revision-yaml";
import { useT } from "@/i18n/use-t";

interface RevisionTabsProps {
  envId: string;
  stackId: string;
  seq: number;
  // The stack's name, for the name of a downloaded file.
  name: string;
}

type Tab = "yaml" | "rendered" | "contents";

// RevisionTabs is what a selected revision was (UI-SPEC "Revision detail"): the YAML difference from the
// deployed revision, the compose file it deployed and its contents. The tab is
// local state and the arrow keys move between tabs. One read feeds them all, so a failed read says so in the
// tab that is open, with Retry.
export function RevisionTabs({ envId, stackId, seq, name }: RevisionTabsProps) {
  const t = useT();
  const [tab, setTab] = useState<Tab>("yaml");
  const revision = useQuery(revisionQuery(envId, stackId, seq));
  const detail = revision.data;

  if (!detail && !revision.isError) {
    return (
      <div
        className="flex flex-col gap-3"
        aria-busy="true"
        role="status"
        aria-label={t("stackRevision.loading", { seq })}
        data-testid="revision-loading"
      >
        <div className="flex gap-2">
          <Skeleton className="h-8 w-24" />
          <Skeleton className="h-8 w-24" />
          <Skeleton className="h-8 w-24" />
        </div>
        <Skeleton className="h-60 w-full" />
      </div>
    );
  }

  function failed(what: "yaml" | "contents") {
    return (
      <Alert variant="destructive" role="alert">
        <AlertDescription className="flex flex-wrap items-center gap-3">
          <span className="font-medium">{t(`stackRevision.error.${what}`)}</span>
          <span>{t("stackRevision.error.body")}</span>
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="max-md:h-11"
            disabled={revision.isFetching}
            onClick={() => void revision.refetch()}
          >
            {t("stackRevision.error.retry")}
          </Button>
        </AlertDescription>
      </Alert>
    );
  }

  return (
    <Tabs value={tab} onValueChange={(v) => setTab(v as Tab)} className="min-w-0 gap-3">
      <TabsList variant="line" aria-label={t("stackRevision.tabs.label", { seq })}>
        <TabsTrigger value="yaml" className="max-md:h-11">
          {t("stackRevision.tabs.yaml")}
        </TabsTrigger>
        <TabsTrigger value="rendered" className="max-md:h-11">
          {t("stackRevision.tabs.rendered")}
        </TabsTrigger>
        <TabsTrigger value="contents" className="max-md:h-11">
          {t("stackRevision.tabs.contents")}
        </TabsTrigger>
      </TabsList>
      <TabsContent value="yaml" className="flex min-w-0 flex-col gap-2">
        {detail ? <RevisionYaml detail={detail} /> : failed("yaml")}
      </TabsContent>
      <TabsContent value="rendered" className="flex min-w-0 flex-col gap-2">
        {!detail ? (
          failed("yaml")
        ) : detail.rendered === "" ? (
          <div className="flex flex-col gap-1 rounded-lg border p-4">
            <p className="font-medium">{t("stackRevision.rendered.empty.title")}</p>
            <p className="text-sm text-muted-foreground">
              {t("stackRevision.rendered.empty.body", { seq })}
            </p>
          </div>
        ) : (
          <RevisionRendered seq={seq} name={name} rendered={detail.rendered} />
        )}
      </TabsContent>
      <TabsContent value="contents" className="flex min-w-0 flex-col gap-2">
        {detail ? <RevisionContents detail={detail} /> : failed("contents")}
      </TabsContent>
    </Tabs>
  );
}
