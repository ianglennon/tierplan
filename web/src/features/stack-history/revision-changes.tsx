// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";

import { revisionChangesQuery } from "@/api/stack-history";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { ServiceDiffList } from "@/features/stack-review/service-diff-list";
import { useT } from "@/i18n/use-t";

interface RevisionChangesProps {
  envId: string;
  stackId: string;
  seq: number;
  // The number of the deployed revision, for the caption while the answer is on its way.
  deployedSeq: number;
}

// RevisionChanges is what rolling back to a revision would change against what runs now (STK-08, D-05): the
// per-service rows the roll back review shows next. The server redacts the live settings for a reader who may
// not reveal secrets, so the rows can be drawn as they come.
export function RevisionChanges({ envId, stackId, seq, deployedSeq }: RevisionChangesProps) {
  const t = useT();
  const changes = useQuery(revisionChangesQuery(envId, stackId, seq));
  const data = changes.data;

  if (!data && !changes.isError) {
    return (
      <div
        className="flex flex-col gap-3"
        aria-busy="true"
        role="status"
        aria-label={t("stackChanges.loading", { seq })}
        data-testid="revision-changes-loading"
      >
        <Skeleton className="h-5 w-2/3" />
        <Skeleton className="h-24 w-full" />
      </div>
    );
  }

  if (!data) {
    return (
      <Alert variant="destructive" role="alert">
        <AlertDescription className="flex flex-wrap items-center gap-3">
          <span className="font-medium">{t("stackChanges.error.title")}</span>
          <span>{t("stackChanges.error.body")}</span>
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="max-md:h-11"
            disabled={changes.isFetching}
            onClick={() => void changes.refetch()}
          >
            {t("stackChanges.error.retry")}
          </Button>
        </AlertDescription>
      </Alert>
    );
  }

  const deployed = data.deployedSeq || deployedSeq;
  return (
    <>
      <p className="text-sm text-muted-foreground">
        {t("stackChanges.caption", { seq, deployed })}
      </p>
      <ServiceDiffList services={data.services ?? []} />
    </>
  );
}
