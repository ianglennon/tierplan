// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { CircleX, History, Info, Undo2 } from "lucide-react";
import type { RefObject } from "react";

import { isRevisionGone, revisionQuery, type StackRevision } from "@/api/stack-history";
import type { StackDetail } from "@/api/stacks";
import { NeedsTooltip } from "@/components/needs-tooltip";
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
import { historyRepoOf, RevisionCommit } from "@/features/stack-history/revision-commit";
import { RevisionLink } from "@/features/stack-history/revision-kind-cell";
import { failureReason } from "@/features/stack-history/revision-result-cell";
import { RevisionTabs } from "@/features/stack-history/revision-tabs";
import { useNeeds } from "@/hooks/use-needs";
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
  // How many revisions the instance keeps of each stack (the history list names it), for the sentence about a
  // revision that was deleted. Without it, the default.
  retention?: number;
}

// How many revisions an instance keeps of each stack unless its settings say otherwise.
const DEFAULT_RETENTION = 20;

// RevisionGone is "That revision is gone": a revision retention deleted, or one that never existed. It names
// how many revisions the instance keeps of each stack. The list says so for a revision it does not hold, and the
// detail says so for one deleted since the list was read, so both show the same card.
export function RevisionGone({
  envId,
  stackId,
  headingRef,
  retention = DEFAULT_RETENTION,
}: {
  envId: string;
  stackId: string;
  headingRef: RefObject<HTMLHeadingElement | null>;
  retention?: number;
}) {
  const t = useT();
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

// RevisionDetail is the card below the table for the selected revision (UI-SPEC "History tab"): its heading
// and who made it and when, then what stands out about it: it failed, it is still deploying, or it is the
// deployed one. The header holds the Roll back action of a revision that finished well and is not the one that
// runs: it opens the review, which is where the roll back is confirmed (D-02). The tabs below show what the
// revision was.
export function RevisionDetail({
  envId,
  stackId,
  item,
  stack,
  headingRef,
  retention = DEFAULT_RETENTION,
}: RevisionDetailProps) {
  const t = useT();
  const needs = useNeeds("stacks.deploy", envId);
  // The same read the tabs make (one request, shared by the query key): when the server says retention
  // deleted this revision since the list was read, the whole card says so.
  const revision = useQuery(revisionQuery(envId, stackId, item.seq));
  if (isRevisionGone(revision.error)) {
    return (
      <RevisionGone envId={envId} stackId={stackId} headingRef={headingRef} retention={retention} />
    );
  }
  const op = stack?.lastOperation;
  const consoleJob = op?.state === "running" ? op.jobId : "";
  // Only a revision that finished well can be rolled back to (D-06), and the deployed one has nothing to roll
  // back (D-51: the server allows it, the page does not offer it).
  const canRollBack = item.outcome === "good" && !item.deployed;
  return (
    <Card className="gap-4 [--card-spacing:--spacing(4)] md:[--card-spacing:--spacing(6)]">
      <CardContent className="flex min-w-0 flex-col gap-4">
        <header className="flex flex-wrap items-start justify-between gap-3">
          <div className="flex min-w-0 flex-col gap-1">
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
              {item.commit ? (
                <>
                  {" · "}
                  {t("stackHistory.detail.commit")}{" "}
                  <RevisionCommit item={item} repo={historyRepoOf(stack)} />
                </>
              ) : null}
            </p>
          </div>
          {canRollBack ? (
            <div className="flex flex-col items-start gap-1 md:items-end">
              {needs.allowed ? (
                <Button asChild className="max-md:h-11">
                  <Link
                    to="/env/$envId/stacks/$stackId/review"
                    params={{ envId, stackId }}
                    search={{ mode: "rollback", revision: item.seq }}
                  >
                    <Undo2 aria-hidden="true" />
                    {t("stackRollback.action", { seq: item.seq })}
                  </Link>
                </Button>
              ) : (
                <NeedsTooltip reason={needs.reason}>
                  <Button type="button" className="max-md:h-11">
                    <Undo2 aria-hidden="true" />
                    {t("stackRollback.action", { seq: item.seq })}
                  </Button>
                </NeedsTooltip>
              )}
              {needs.allowed && stack?.draftSavedAt ? (
                <p className="text-xs text-muted-foreground">{t("stackRollback.draftNote")}</p>
              ) : null}
              {stack?.git?.autoDeploy ? (
                <p className="text-xs text-muted-foreground">
                  {t("stackHistory.detail.rollbackPauses")}
                </p>
              ) : null}
            </div>
          ) : null}
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
