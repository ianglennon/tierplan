// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Link } from "@tanstack/react-router";

import { AgentUpdateNotice } from "@/components/agent-update-notice";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import type { PendingVerb } from "@/features/containers/pending";
import { useCanReadAudit } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

// One container's part of a bulk: what happened to it and, when it did not
// work, why in the words of the UI (never a code).
export interface BulkReportRow {
  name: string;
  node: string;
  result: "done" | "failed" | "skipped";
  reason: string;
  // The code behind a failure, kept so the dialog can tell an agent that needs an
  // update from the rest.
  code?: string;
}

export interface BulkReport {
  verb: PendingVerb;
  // The words of a bulk that is not about containers: the dialog's title, and the
  // header of the first column ("Volume"). Without them the dialog reads "{Verb}
  // results" over a Container column.
  title?: string;
  itemLabel?: string;
  // The ID the audit rows of this operation share (D-14), when the server made
  // one; it is what the link to the audit log filters by.
  bulkId?: string | undefined;
  rows: BulkReportRow[];
}

interface BulkResultDialogProps {
  envId: string;
  // Open while there is a report.
  report: BulkReport | null;
  onClose: () => void;
}

// BulkResultDialog is "Stop results": a table of Container, Node, Result and
// Reason for a bulk that partly failed, opened from the toast's Details. Names
// and reasons are text only; the table is keyboard navigable by being a table.
export function BulkResultDialog({ envId, report, onClose }: BulkResultDialogProps) {
  const t = useT();
  const canReadAudit = useCanReadAudit();
  const missing = t("dashboard.time.missing");
  const title =
    report?.title ??
    t("actions.results.title", {
      verb: report ? t(`actions.results.verb.${report.verb}`) : "",
    });
  const agentOld = report?.rows.some((r) => r.code === "agent_policy_denied") ?? false;
  return (
    <Dialog open={report !== null} onOpenChange={(open) => !open && onClose()}>
      <DialogContent showCloseButton={false} className="max-h-[90svh] overflow-y-auto sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>{title}</DialogTitle>
          <DialogDescription className="sr-only">{title}</DialogDescription>
        </DialogHeader>
        {agentOld ? <AgentUpdateNotice envId={envId} /> : null}
        <div className="max-h-[60svh] overflow-auto rounded-lg ring-1 ring-foreground/10">
          <table className="w-full text-sm">
            <thead className="sticky top-0 bg-muted text-xs text-muted-foreground">
              <tr>
                {(["container", "node", "result", "reason"] as const).map((c) => (
                  <th key={c} scope="col" className="px-2 py-1 text-start font-semibold">
                    {c === "container" && report?.itemLabel
                      ? report.itemLabel
                      : t(`actions.results.${c}`)}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {(report?.rows ?? []).map((r, i) => (
                <tr
                  // The same name can be on two nodes.
                  // biome-ignore lint/suspicious/noArrayIndexKey: a static list with no identity beyond its place
                  key={i}
                  className={cn(
                    "border-t align-top",
                    r.result === "skipped" && "text-muted-foreground",
                  )}
                >
                  <td className="px-2 py-1 font-mono text-xs break-all">{r.name}</td>
                  <td className="px-2 py-1 break-words">{r.node}</td>
                  <td
                    className={cn(
                      "px-2 py-1 whitespace-nowrap",
                      r.result === "failed" && "text-destructive",
                    )}
                  >
                    {t(`actions.results.${r.result}`)}
                  </td>
                  <td className="px-2 py-1 break-words">{r.reason || missing}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        <DialogFooter>
          {report?.bulkId && canReadAudit ? (
            <Button asChild variant="outline" className="max-md:h-11">
              <Link to="/audit" search={{ bulk: report.bulkId }} onClick={onClose}>
                {t("actions.results.audit")}
              </Link>
            </Button>
          ) : null}
          <Button type="button" className="max-md:h-11" onClick={onClose}>
            {t("actions.results.close")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
