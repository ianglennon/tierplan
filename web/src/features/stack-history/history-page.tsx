// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { History } from "lucide-react";

import { revisionsQuery } from "@/api/stack-history";
import { stackQuery } from "@/api/stacks";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@/components/ui/empty";
import { Skeleton } from "@/components/ui/skeleton";
import { RevisionTable } from "@/features/stack-history/revision-table";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";

interface HistoryPageProps {
  envId: string;
  stackId: string;
  // The revision number in the address, if it names one.
  rev: number | undefined;
}

// HistoryPage is the History tab (UI-SPEC "History tab"): the revisions of the stack newest first, then the
// selected revision. With no revision in the address the deployed one is selected, and when none is
// deployed the newest.
export function HistoryPage({ envId, stackId, rev }: HistoryPageProps) {
  const t = useT();
  const stack = useQuery(stackQuery(envId, stackId));
  const history = useQuery(revisionsQuery(envId, stackId));
  const deploy = useNeeds("stacks.deploy", envId);
  const name = stack.data?.name ?? stackId;
  const items = history.data?.items ?? [];

  if (history.isError && !history.data) {
    return (
      <Alert variant="destructive">
        <AlertDescription className="flex flex-wrap items-center gap-3">
          <span>{t("stackHistory.error.title")}</span>
          <span>{t("stackHistory.error.body")}</span>
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="max-md:h-11"
            disabled={history.isFetching}
            onClick={() => void history.refetch()}
          >
            {t("stackHistory.error.retry")}
          </Button>
        </AlertDescription>
      </Alert>
    );
  }

  if (!history.data) {
    return (
      <div className="flex flex-col gap-4" aria-busy="true" data-testid="history-loading">
        {Array.from({ length: 8 }, (_, i) => (
          // The rows are placeholders in a fixed order.
          // biome-ignore lint/suspicious/noArrayIndexKey: placeholders have no identity
          <Skeleton key={i} className="h-9 w-full" />
        ))}
        <Skeleton className="h-40 w-full" />
      </div>
    );
  }

  if (items.length === 0) {
    return (
      <Empty>
        <EmptyHeader>
          <EmptyMedia variant="icon">
            <History aria-hidden="true" />
          </EmptyMedia>
          <EmptyTitle role="heading" aria-level={2}>
            {t("stackHistory.empty.title")}
          </EmptyTitle>
          <EmptyDescription>{t("stackHistory.empty.body", { name })}</EmptyDescription>
        </EmptyHeader>
        {deploy.allowed ? (
          <EmptyContent>
            <Button asChild variant="outline" className="max-md:h-11">
              <Link to="/env/$envId/stacks/$stackId/review" params={{ envId, stackId }}>
                {t("stackHistory.empty.action")}
              </Link>
            </Button>
          </EmptyContent>
        ) : null}
      </Empty>
    );
  }

  const selected =
    items.find((i) => i.seq === rev) ??
    (rev === undefined ? (items.find((i) => i.deployed) ?? items[0]) : undefined);

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-col gap-1">
        <p className="text-sm text-muted-foreground">{t("stackHistory.caption", { name })}</p>
        <p className="text-sm text-muted-foreground">
          {t("stackHistory.count", { count: items.length })}
        </p>
      </div>
      <RevisionTable
        envId={envId}
        stackId={stackId}
        name={name}
        items={items}
        selected={selected?.seq}
      />
      {selected ? (
        <h3 className="text-base font-semibold">
          {t("stackHistory.detail.heading", { seq: selected.seq })}
        </h3>
      ) : null}
    </div>
  );
}
