// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import type { StackDriftService } from "@/api/stack-drift";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { containersOf } from "@/features/stack-drift/drift-service-list";
import { useT } from "@/i18n/use-t";

interface DriftSummaryProps {
  services: StackDriftService[];
  // A standalone host drifts by its containers, a swarm by its services.
  target: "swarm" | "standalone";
}

// DriftSummary is the Summary of drift (UI-SPEC "Drift"): a native table with a column for each way a service can
// drift and a Services row. A standalone host's services are run by containers, so it has a Containers row as well,
// which counts the containers of the services in each column. The words are real text.
export function DriftSummary({ services, target }: DriftSummaryProps) {
  const t = useT();
  const count = (change: StackDriftService["change"]) =>
    services.filter((s) => s.change === change).length;
  const containers = (change: StackDriftService["change"]) =>
    services.filter((s) => s.change === change).reduce((n, s) => n + containersOf(s), 0);
  return (
    <div className="rounded-lg ring-1 ring-foreground/10">
      <Table>
        <caption className="sr-only">{t("stackDrift.summary.caption")}</caption>
        <TableHeader>
          <TableRow>
            <TableHead scope="col" aria-label={t("stackDrift.summary.kind")} className="w-40" />
            <TableHead scope="col">{t("stackDrift.summary.changed")}</TableHead>
            <TableHead scope="col">{t("stackDrift.summary.missing")}</TableHead>
            <TableHead scope="col">{t("stackDrift.summary.extra")}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          <TableRow>
            <TableHead scope="row" className="font-medium">
              {t("stackDrift.summary.services")}
            </TableHead>
            <TableCell>{count("changed")}</TableCell>
            <TableCell>{count("missing")}</TableCell>
            <TableCell>{count("extra")}</TableCell>
          </TableRow>
          {target === "standalone" ? (
            <TableRow>
              <TableHead scope="row" className="font-medium">
                {t("stackDrift.summary.containers")}
              </TableHead>
              <TableCell>{containers("changed")}</TableCell>
              <TableCell>{containers("missing")}</TableCell>
              <TableCell>{containers("extra")}</TableCell>
            </TableRow>
          ) : null}
        </TableBody>
      </Table>
    </div>
  );
}
