// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { StackReviewSummary } from "@/api/stacks";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { SettingsCard } from "@/features/environments/settings-card";
import { useT } from "@/i18n/use-t";

// The rows of the table, in the order the UI-SPEC lists them.
const ROWS = ["services", "networks", "volumes", "secrets", "configs"] as const;

// Secrets and configs are named by a hash of their content: a changed one is a new object, which
// the table counts as Changed and says so (UI-SPEC "Review", D-07).
const HASHED = new Set<(typeof ROWS)[number]>(["secrets", "configs"]);

interface ReviewSummaryProps {
  // The server's counts; absent while the review is on its way, when the rows are skeletons.
  summary?: StackReviewSummary;
}

// ReviewSummary is the Summary card of the review: counts only, a native table with a caption,
// one row for each kind of thing a stack holds and a last row for the images. Zero counts read
// 0, never blank (UI-SPEC "Shared state and overflow rules").
export function ReviewSummary({ summary }: ReviewSummaryProps) {
  const t = useT();
  return (
    <SettingsCard title={t("stackReview.summary.title")}>
      {summary ? (
        <>
          <Table>
            <caption className="sr-only">{t("stackReview.summary.caption")}</caption>
            <TableHeader>
              <TableRow>
                <TableHead scope="col">{t("stackReview.summary.resource")}</TableHead>
                <TableHead scope="col" className="text-end">
                  {t("stackReview.summary.added")}
                </TableHead>
                <TableHead scope="col" className="text-end">
                  {t("stackReview.summary.changed")}
                </TableHead>
                <TableHead scope="col" className="text-end">
                  {t("stackReview.summary.removed")}
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {ROWS.map((row) => {
                const counts = summary[row];
                return (
                  <TableRow key={row}>
                    <TableHead scope="row" className="font-medium">
                      {t(`stackReview.summary.${row}`)}
                    </TableHead>
                    <TableCell className="text-end tabular-nums">{counts.added}</TableCell>
                    <TableCell className="text-end tabular-nums">
                      {counts.changed}
                      {HASHED.has(row) && counts.changed > 0 ? (
                        <span className="block text-xs text-muted-foreground">
                          {t("stackReview.summary.newVersions", { count: counts.changed })}
                        </span>
                      ) : null}
                    </TableCell>
                    <TableCell className="text-end tabular-nums">{counts.removed}</TableCell>
                  </TableRow>
                );
              })}
              <TableRow>
                <TableHead scope="row" className="font-medium">
                  {t("stackReview.summary.images")}
                </TableHead>
                <TableCell colSpan={3} className="text-end">
                  {t("stackReview.summary.imagesCount", {
                    pinned: summary.imagesPinned,
                    new: summary.imagesNew,
                  })}
                </TableCell>
              </TableRow>
            </TableBody>
          </Table>
          {summary.ownershipOnly > 0 ? (
            <p className="text-sm text-muted-foreground">
              {t("stackOwnership.summary.line", { count: summary.ownershipOnly })}
            </p>
          ) : null}
        </>
      ) : (
        <div data-testid="summary-skeleton" aria-hidden="true" className="flex flex-col gap-2">
          {ROWS.map((row) => (
            <Skeleton key={row} className="h-9 w-full" />
          ))}
        </div>
      )}
    </SettingsCard>
  );
}
