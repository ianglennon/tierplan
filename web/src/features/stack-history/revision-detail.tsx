// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Link } from "@tanstack/react-router";
import { CircleX, Info } from "lucide-react";
import type { RefObject } from "react";

import type { StackRevision } from "@/api/stack-history";
import type { StackDetail } from "@/api/stacks";
import { RelativeTime } from "@/components/relative-time";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
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
}

// RevisionDetail is the card below the table for the selected revision (UI-SPEC "History tab"): its heading
// and who made it and when, then what stands out about it: it failed, it is still deploying, or it is the
// deployed one. The tabs below show what the revision was; the Roll back action is added by the plan that follows.
export function RevisionDetail({ envId, stackId, item, stack, headingRef }: RevisionDetailProps) {
  const t = useT();
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
