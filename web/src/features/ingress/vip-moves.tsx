// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useInfiniteQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { ingressVIPMovesQuery } from "@/api/ingress-nodes";
import { RelativeTime } from "@/components/relative-time";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { useT } from "@/i18n/use-t";

const REASONS = new Set(["holder_failed", "probe_failing", "drain_handoff", "disabled"]);

// VIPMoves is the history of the virtual IP's moves under the node table (UI-SPEC S4, D-06): the newest ten, then
// "Show more" reads the next, older page from the cursor and appends it. The button is gone on the last page.
export function VIPMoves({ envId }: { envId: string }) {
  const t = useT();
  const query = useInfiniteQuery(ingressVIPMovesQuery(envId));
  const moves = (query.data?.pages ?? []).flatMap((p) => p.items ?? []);
  const failed = query.isError && moves.length === 0;

  let body: ReactNode;
  if (query.isPending) {
    body = (
      <div>
        <span role="status" className="sr-only">
          {t("ingressPage.nodes.moves.loading")}
        </span>
        <Skeleton className="h-24 w-full" aria-hidden="true" />
      </div>
    );
  } else if (failed) {
    body = (
      <Alert variant="destructive">
        <AlertDescription className="flex flex-wrap items-center gap-3">
          <span>{t("ingressPage.nodes.moves.error")}</span>
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="max-md:h-11"
            disabled={query.isFetching}
            onClick={() => void query.refetch()}
          >
            {t("ingressPage.nodes.moves.retry")}
          </Button>
        </AlertDescription>
      </Alert>
    );
  } else if (moves.length === 0) {
    body = <p className="text-sm text-muted-foreground">{t("ingressPage.nodes.moves.empty")}</p>;
  } else {
    body = (
      <>
        <Table aria-label={t("ingressPage.nodes.moves.label")}>
          <TableHeader>
            <TableRow>
              <TableHead scope="col">{t("ingressPage.nodes.moves.columns.when")}</TableHead>
              <TableHead scope="col">{t("ingressPage.nodes.moves.columns.move")}</TableHead>
              <TableHead scope="col">{t("ingressPage.nodes.moves.columns.why")}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {moves.map((m, i) => (
              // A move has no ID of its own: the instant and the two sides tell two apart, and the index keeps a
              // repeat of all three apart.
              // biome-ignore lint/suspicious/noArrayIndexKey: see above
              <TableRow key={`${m.at}-${m.fromNodeId}-${m.toNodeId}-${i}`}>
                <TableCell className="align-top text-sm whitespace-normal">
                  <RelativeTime value={m.at} />
                </TableCell>
                <TableCell className="align-top text-sm break-words whitespace-normal">
                  {t("ingressPage.nodes.moves.move", {
                    from: m.fromNode || m.fromNodeId || t("ingressPage.nodes.moves.noNode"),
                    to: m.toNode || m.toNodeId || t("ingressPage.nodes.moves.noNode"),
                  })}
                </TableCell>
                <TableCell className="align-top text-sm whitespace-normal">
                  {t(
                    `ingressPage.nodes.moves.reason.${REASONS.has(m.reason) ? m.reason : "unknown"}`,
                  )}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
        {query.hasNextPage ? (
          <div>
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="max-md:h-11"
              disabled={query.isFetchingNextPage}
              onClick={() => void query.fetchNextPage()}
            >
              {t("ingressPage.nodes.moves.more")}
            </Button>
          </div>
        ) : null}
      </>
    );
  }

  return (
    <section className="flex flex-col gap-3">
      <h3 className="text-base font-semibold">{t("ingressPage.nodes.moves.title")}</h3>
      {body}
    </section>
  );
}
