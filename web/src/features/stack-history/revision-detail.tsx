// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { CircleX, History, Info } from "lucide-react";
import type { RefObject } from "react";

import { isRevisionGone, revisionQuery, type StackRevision } from "@/api/stack-history";
import type { StackDetail } from "@/api/stacks";
import { RelativeTime } from "@/components/relative-time";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import {
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@/components/ui/empty";
import { RevisionLink } from "@/features/stack-history/revision-kind-cell";
import { failureReason } from "@/features/stack-history/revision-result-cell";
import { RevisionTabs } from "@/features/stack-history/revision-tabs";
import { useT } from "@/i18n/use-t";

interface RevisionDetailProps {
  envId: string;
  stackId: string;
  item: StackRevision;
  // The stack, once it has loaded: the way to the console of a deploy that is still running.
  stack: StackDetail | undefined;
  // The heading takes focus when a revision is selected, so a keyboard or screen reader user lands on what
  // they chose (UI-SPEC "Interaction and Accessibility Contract").
  headingRef: RefObject<HTMLHeadingElement | null>;
  // How many revisions the instance keeps of each stack, for the sentence about a revision that was deleted.
  // Until the retention setting is passed in (plan 08-06) it is the default.
  retention?: number;
}

// How many revisions an instance keeps of each stack unless its settings say otherwise.
const DEFAULT_RETENTION = 20;

// RevisionDetail is the card below the table for the selected revision (UI-SPEC "History tab"): its heading
// and who made it and when, then what stands out about it: it failed, it is still deploying, or it is the
// deployed one. The tabs below show what the revision was; the Roll back action is added by the plan that follows.
export function RevisionDetail({
  envId,
  stackId,
  item,
  stack,
  headingRef,
  retention = DEFAULT_RETENTION,
}: RevisionDetailProps) {
  const t = useT();
  // The same read the tabs make (one request, shared by the query key): when the server says retention
  // deleted this revision since the list was read, the whole card says so.
  const revision = useQuery(revisionQuery(envId, stackId, item.seq));
  if (isRevisionGone(revision.error)) {
    return (
      <Empty>
        <EmptyHeader>
          <EmptyMedia variant="icon">
            <History aria-hidden="true" />
          </EmptyMedia>
          <EmptyTitle role="heading" aria-level={3} ref={headingRef} tabIndex={-1}>
            {t("stackRevision.gone.title")}
          </EmptyTitle>
          <EmptyDescription>{t("stackRevision.gone.body", { count: retention })}</EmptyDescription>
        </EmptyHeader>
        <EmptyContent>
          <Button asChild variant="outline" className="max-md:h-11">
            <Link to="/env/$envId/stacks/$stackId/history" params={{ envId, stackId }} replace>
              {t("stackRevision.gone.back")}
            </Link>
          </Button>
        </EmptyContent>
      </Empty>
    );
  }
  const op = stack?.lastOperation;
  const consoleJob = op?.state === "running" ? op.jobId : "";
  return (
    <Card className="gap-4 [--card-spacing:--spacing(4)] md:[--card-spacing:--spacing(6)]">
      <CardContent className="flex min-w-0 flex-col gap-4">
        <header className="flex flex-col gap-1">
          <h3 ref={headingRef} tabIndex={-1} className="text-base font-semibold outline-none">
            {t("stackHistory.detail.heading")} <span className="font-mono">#{item.seq}</span>
          </h3>
          <p className="text-sm text-muted-foreground">
            {item.origin === "rollback" && item.restoredFrom > 0 ? (
              <>
                {t("stackHistory.detail.rollbackOf")}{" "}
                <RevisionLink envId={envId} stackId={stackId} seq={item.restoredFrom} />
              </>
            ) : (
              t(`stackHistory.kind.${item.origin}`)
            )}{" "}
            {t("stackHistory.detail.by", { user: item.createdBy || t("stackHistory.missing") })}{" "}
            <RelativeTime value={item.createdAt} />
          </p>
        </header>
        {item.outcome === "failed" ? (
          <Alert variant="destructive" role="alert">
            <CircleX aria-hidden="true" />
            <AlertTitle>{t("stackHistory.detail.failed.title", { seq: item.seq })}</AlertTitle>
            <AlertDescription className="flex flex-col gap-1">
              <p className="break-all">
                {item.failedService
                  ? t("stackHistory.detail.failed.body", {
                      service: item.failedService,
                      reason: failureReason(t, item),
                    })
                  : failureReason(t, item)}
              </p>
              <p>{t("stackHistory.detail.failed.keep")}</p>
            </AlertDescription>
          </Alert>
        ) : null}
        {item.outcome === "pending" ? (
          <Alert>
            <Info aria-hidden="true" />
            <AlertTitle>{t("stackHistory.detail.pending.title", { seq: item.seq })}</AlertTitle>
            {consoleJob ? (
              <AlertDescription>
                <Button asChild variant="outline" size="sm" className="max-md:h-11">
                  <Link
                    to="/env/$envId/stacks/$stackId/deploys/$jobId"
                    params={{ envId, stackId, jobId: consoleJob }}
                  >
                    {t("stackHistory.detail.pending.console")}
                  </Link>
                </Button>
              </AlertDescription>
            ) : null}
          </Alert>
        ) : null}
        {item.deployed ? (
          <Alert>
            <Info aria-hidden="true" />
            <AlertDescription>{t("stackHistory.detail.deployed")}</AlertDescription>
          </Alert>
        ) : null}
        <RevisionTabs
          envId={envId}
          stackId={stackId}
          seq={item.seq}
          name={stack?.name ?? stackId}
        />
      </CardContent>
    </Card>
  );
}
