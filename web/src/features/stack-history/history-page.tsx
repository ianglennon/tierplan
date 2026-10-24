// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { History } from "lucide-react";
import { useEffect, useRef, useState } from "react";

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
import { RevisionDetail } from "@/features/stack-history/revision-detail";
import { RevisionTable } from "@/features/stack-history/revision-table";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";

interface HistoryPageProps {
  envId: string;
  stackId: string;
  // The revision number in the address, if it names one.
  rev: number | undefined;
}

// How long a revision that appeared while the tab was open keeps its highlight (P4 D-05).
const FRESH_MS = 1500;

// useFreshRevisions is the numbers of revisions that appeared after the list first loaded, for 1.5 s each.
// The first list is not highlighted, and neither is one that replaces the list of another stack.
function useFreshRevisions(seqs: readonly number[] | undefined): ReadonlySet<number> {
  const seen = useRef<Set<number> | null>(null);
  const [fresh, setFresh] = useState<ReadonlySet<number>>(new Set());
  useEffect(() => {
    if (!seqs) return;
    if (seen.current === null) {
      seen.current = new Set(seqs);
      return;
    }
    const known = seen.current;
    const added = seqs.filter((s) => !known.has(s));
    if (added.length === 0) return;
    for (const s of added) known.add(s);
    setFresh((prev) => new Set([...prev, ...added]));
    const id = setTimeout(
      () =>
        setFresh((prev) => {
          const next = new Set(prev);
          for (const s of added) next.delete(s);
          return next;
        }),
      FRESH_MS,
    );
    return () => clearTimeout(id);
  }, [seqs]);
  return fresh;
}

// HistoryPage is the History tab (UI-SPEC "History tab"): the revisions of the stack newest first, then the
// selected revision. With no revision in the address the deployed one is selected, and when none is
// deployed the newest. A revision the list does not hold is gone.
export function HistoryPage({ envId, stackId, rev }: HistoryPageProps) {
  const t = useT();
  const stack = useQuery(stackQuery(envId, stackId));
  const history = useQuery(revisionsQuery(envId, stackId));
  const deploy = useNeeds("stacks.deploy", envId);
  const name = stack.data?.name ?? stackId;
  const items = history.data?.items ?? null;
  const headingRef = useRef<HTMLHeadingElement>(null);
  const fresh = useFreshRevisions(items?.map((i) => i.seq));

  // Choosing a revision moves focus to its heading. The first render, which opens the tab, does not.
  const firstRev = useRef(true);
  // biome-ignore lint/correctness/useExhaustiveDependencies: the focus follows the address, nothing else
  useEffect(() => {
    if (firstRev.current) {
      firstRev.current = false;
      return;
    }
    headingRef.current?.focus();
  }, [rev]);

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

  if (!items) {
    return (
      <div className="flex flex-col gap-4" aria-busy="true" data-testid="history-loading">
        <div className="flex flex-col gap-1">
          {Array.from({ length: 8 }, (_, i) => (
            // biome-ignore lint/suspicious/noArrayIndexKey: placeholders have no identity
            <Skeleton key={i} className="h-9 w-full" />
          ))}
        </div>
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

  // An address that names a revision selects it, and one the list does not hold is gone. With no revision
  // in the address, the deployed one is selected, else the newest.
  const selected =
    rev === undefined
      ? (items.find((i) => i.deployed) ?? items[0])
      : items.find((i) => i.seq === rev);

  return (
    <div className="flex flex-col gap-8">
      <section className="flex flex-col gap-4">
        <div className="flex flex-col gap-1">
          <p className="text-sm text-muted-foreground">{t("stackHistory.caption", { name })}</p>
          <p className="text-sm text-muted-foreground">
            {t("stackHistory.count", { count: items.length })}
            {history.data?.capped ? ` · ${t("stackHistory.capped", { count: items.length })}` : ""}
          </p>
        </div>
        <RevisionTable
          envId={envId}
          stackId={stackId}
          name={name}
          items={items}
          selected={selected?.seq}
          fresh={fresh}
        />
      </section>
      {selected ? (
        <RevisionDetail
          envId={envId}
          stackId={stackId}
          item={selected}
          stack={stack.data}
          headingRef={headingRef}
        />
      ) : (
        <Empty>
          <EmptyHeader>
            <EmptyMedia variant="icon">
              <History aria-hidden="true" />
            </EmptyMedia>
            <EmptyTitle role="heading" aria-level={3} ref={headingRef} tabIndex={-1}>
              {t("stackHistory.gone.title")}
            </EmptyTitle>
            <EmptyDescription>{t("stackHistory.gone.body")}</EmptyDescription>
          </EmptyHeader>
          <EmptyContent>
            <Button asChild variant="outline" className="max-md:h-11">
              <Link to="/env/$envId/stacks/$stackId/history" params={{ envId, stackId }} replace>
                {t("stackHistory.gone.back")}
              </Link>
            </Button>
          </EmptyContent>
        </Empty>
      )}
    </div>
  );
}
