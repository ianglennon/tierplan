// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { TriangleAlert } from "lucide-react";

import type { NodeLabelImpact, NodeLabelImpactRow } from "@/api/nodes";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Spinner } from "@/components/ui/spinner";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { useT } from "@/i18n/use-t";

export interface LabelImpactPanelProps {
  // idle is nothing to check (nothing is changed); pending is a check that is not answered yet,
  // including the wait before it is asked; ready has the answer; error could not get one.
  status: "idle" | "pending" | "ready" | "error";
  impact?: NodeLabelImpact;
  onRetry?: () => void;
}

type Translate = ReturnType<typeof useT>;

// What a constraint does to the node after the change. A warning icon goes with the words for a
// constraint the node stops meeting, so the sign is never colour alone (UI-SPEC Color).
function After({ row, t }: { row: NodeLabelImpactRow; t: Translate }) {
  if (row.before && !row.after) {
    return (
      <span className="inline-flex items-center gap-1.5">
        <TriangleAlert aria-hidden="true" className="size-4 shrink-0 text-status-warning-text" />
        {t("nodes.labels.impact.noLongerMatches")}
      </span>
    );
  }
  if (!row.before && row.after) return <>{t("nodes.labels.impact.nowMatches")}</>;
  return (
    <>{row.after ? t("nodes.labels.impact.matches") : t("nodes.labels.impact.doesNotMatch")}</>
  );
}

// LabelImpactPanel is the Effect on services of the Labels tab (UI-SPEC "Nodes", D-21): which
// services have a placement constraint on a label the edit touches, whether the node meets it
// before and after, and how many tasks the swarm may move off the node. Service names and
// constraints are Docker's text and are drawn as text only.
export function LabelImpactPanel({ status, impact, onRetry }: LabelImpactPanelProps) {
  const t = useT();
  const rows = status === "ready" && impact ? (impact.rows ?? []) : [];
  return (
    <section aria-labelledby="node-label-impact-heading" className="flex flex-col gap-3">
      <h3 id="node-label-impact-heading" className="text-base font-semibold">
        {t("nodes.labels.impact.title")}
      </h3>

      {status === "pending" ? (
        <div role="status" className="flex items-center gap-2 text-sm text-muted-foreground">
          <Spinner aria-hidden="true" />
          <span>{t("nodes.labels.impact.checking")}</span>
        </div>
      ) : null}

      {status === "error" ? (
        <Alert variant="destructive" role="alert">
          <AlertDescription className="flex flex-wrap items-center gap-3">
            <span>{t("nodes.labels.impact.failed")}</span>
            {onRetry ? (
              <Button type="button" variant="outline" size="sm" onClick={onRetry}>
                {t("nodes.labels.impact.retry")}
              </Button>
            ) : null}
          </AlertDescription>
        </Alert>
      ) : null}

      {(status === "idle" || status === "ready") && rows.length === 0 ? (
        <p className="text-sm text-muted-foreground">{t("nodes.labels.impact.empty")}</p>
      ) : null}

      {rows.length > 0 ? (
        <>
          <Table aria-label={t("nodes.labels.impact.title")}>
            <TableHeader>
              <TableRow>
                <TableHead>{t("nodes.labels.impact.service")}</TableHead>
                <TableHead>{t("nodes.labels.impact.constraint")}</TableHead>
                <TableHead>{t("nodes.labels.impact.before")}</TableHead>
                <TableHead>{t("nodes.labels.impact.after")}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {rows.map((row) => (
                <TableRow key={`${row.serviceId}\u0000${row.constraint}`}>
                  <TableCell className="whitespace-normal break-all">{row.service}</TableCell>
                  <TableCell className="whitespace-normal break-all font-mono text-xs">
                    {row.constraint}
                  </TableCell>
                  <TableCell>
                    {row.before
                      ? t("nodes.labels.impact.matches")
                      : t("nodes.labels.impact.doesNotMatch")}
                  </TableCell>
                  <TableCell>
                    <After row={row} t={t} />
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
          {impact && impact.tasksMayMove > 0 ? (
            <p className="text-sm">
              {t("nodes.labels.impact.summary", { count: impact.tasksMayMove })}
            </p>
          ) : null}
        </>
      ) : null}
    </section>
  );
}
