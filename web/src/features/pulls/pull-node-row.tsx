// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Link } from "@tanstack/react-router";
import { Check, ChevronRight, Copy } from "lucide-react";
import { useId, useState } from "react";

import type { PullItem } from "@/api/pulls";
import { AgentUpdateNotice } from "@/components/agent-update-notice";
import { copyText } from "@/components/copyable-code";
import { Button } from "@/components/ui/button";
import { Progress } from "@/components/ui/progress";
import {
  Table,
  TableBody,
  TableCaption,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { type FailureCredential, failureCopy } from "@/features/pulls/failure-copy";
import { usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { formatBytes } from "@/lib/format";
import { toastError } from "@/lib/toast";
import { cn } from "@/lib/utils";

// shortDigest keeps the algorithm and the first twelve characters: sha256:0123456789ab.
export function shortDigest(digest: string): string {
  const [algo, hex] = digest.split(":");
  return hex ? `${algo}:${hex.slice(0, 12)}` : digest.slice(0, 19);
}

type Display = "waiting" | "running" | "done" | "failed" | "cancelled";

const DOT: Record<Display, string> = {
  waiting: "bg-status-unknown",
  running: "bg-status-warning animate-pulse motion-reduce:animate-none",
  done: "bg-status-online",
  failed: "bg-status-offline",
  cancelled: "bg-status-unknown",
};

const TEXT: Record<Display, string> = {
  waiting: "text-muted-foreground",
  running: "text-status-warning-text",
  done: "text-status-online-text",
  failed: "text-status-offline-text",
  cancelled: "text-muted-foreground",
};

// What a row says it is doing: a node that has started but whose total is not known yet is
// still Waiting, because the bar has nothing to measure (UI-SPEC "Progress").
function displayOf(item: PullItem): Display {
  if (item.state === "running" && item.total <= 0) return "waiting";
  return item.state;
}

interface PullNodeRowProps {
  item: PullItem;
  // The normalised reference being pulled.
  imageRef: string;
  envId: string;
  // The credential the pull used, null when it was anonymous; failures name it.
  credential?: FailureCredential | null;
  // The environment's name, said as the scope of an environment credential.
  envName?: string;
}

// PullNodeRow is one node of a pull: its state, its bar with the amount beside it, the
// digest once it is done or the reason it failed, and the layers on request.
export function PullNodeRow({ item, imageRef, envId, credential, envName }: PullNodeRowProps) {
  const t = useT();
  const canAdd = usePermission("registries.instance");
  const panelId = useId();
  const [open, setOpen] = useState(false);
  const [copied, setCopied] = useState(false);
  const display = displayOf(item);
  const layers = item.layers ?? [];
  const known = item.total > 0;
  const percent = known
    ? Math.min(100, Math.max(0, Math.round((item.current / item.total) * 100)))
    : item.state === "done"
      ? 100
      : null;
  const failed = item.state === "failed" || item.state === "cancelled";
  const agentPolicy = item.code === "agent_policy_denied";
  const failure = failed
    ? failureCopy(t, item.code, {
        credential: credential ?? null,
        envName,
        ref: imageRef,
        node: item.name,
      })
    : null;
  // Add credential is for whoever may add one; Test credential opens one the person can see.
  const action =
    failure?.action && (failure.action.kind === "test" || canAdd) ? failure.action : null;

  async function copyDigest() {
    if (await copyText(item.digest)) {
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    } else {
      toastError(t("env.copy.failed"));
    }
  }

  return (
    <li className="flex flex-col gap-2 rounded-lg border p-3">
      <div className="flex items-center gap-2">
        <Button
          type="button"
          variant="ghost"
          size="icon-sm"
          className="shrink-0 max-md:size-11"
          aria-expanded={open}
          aria-controls={panelId}
          aria-label={t(open ? "pulls.node.collapse" : "pulls.node.expand", { node: item.name })}
          onClick={() => setOpen((v) => !v)}
        >
          <ChevronRight
            aria-hidden="true"
            className={cn(
              "transition-transform motion-reduce:transition-none",
              open && "rotate-90",
            )}
          />
        </Button>
        <span className="min-w-0 flex-1 truncate font-medium">{item.name}</span>
        <span className={cn("flex shrink-0 items-center gap-1.5 text-sm", TEXT[display])}>
          <span aria-hidden="true" className={cn("size-2 rounded-full", DOT[display])} />
          {t(`pulls.node.status.${display}`)}
        </span>
      </div>

      <div className="flex items-center gap-3">
        <Progress
          className="flex-1"
          value={percent}
          aria-label={t("pulls.node.progressLabel", { node: item.name })}
        />
        <span className="min-w-0 shrink-0 text-right text-xs text-muted-foreground tabular-nums">
          {known
            ? t("pulls.node.bytes", {
                done: formatBytes(item.current),
                total: formatBytes(item.total),
              })
            : null}
          {known && item.count > 0 ? " · " : null}
          {item.count > 0 ? t("pulls.node.layers", { count: item.count, done: item.done }) : null}
        </span>
      </div>

      {item.state === "done" ? (
        <div className="flex flex-wrap items-center gap-2 text-sm">
          {item.digest ? (
            <>
              <Tooltip>
                <TooltipTrigger asChild>
                  <span className="font-mono text-xs break-all">
                    {t("pulls.node.digest", { digest: shortDigest(item.digest) })}
                  </span>
                </TooltipTrigger>
                <TooltipContent className="font-mono break-all">{item.digest}</TooltipContent>
              </Tooltip>
              <Button
                type="button"
                variant="ghost"
                size="icon-sm"
                className="max-md:size-11"
                aria-label={t("pulls.node.copyDigest")}
                onClick={copyDigest}
              >
                {copied ? <Check aria-hidden="true" /> : <Copy aria-hidden="true" />}
              </Button>
            </>
          ) : null}
          {item.upToDate ? (
            <span className="text-muted-foreground">{t("pulls.node.upToDate")}</span>
          ) : null}
        </div>
      ) : null}

      {failed && agentPolicy ? <AgentUpdateNotice envId={envId} /> : null}
      {failed && !agentPolicy && failure ? (
        // A node the person cancelled is told in plain words, not as an alarm.
        <div
          role={item.state === "cancelled" ? "status" : "alert"}
          className={cn(
            "flex flex-col gap-1 text-sm break-words",
            item.state === "cancelled" ? "text-muted-foreground" : "text-status-offline-text",
          )}
        >
          <p>{failure.text}</p>
          {action ? (
            <p>
              <Link
                to={action.to}
                search={action.search}
                className="inline-flex rounded-sm text-accent-ink underline underline-offset-3 focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none max-md:min-h-11 max-md:items-center"
              >
                {action.label}
              </Link>
            </p>
          ) : null}
        </div>
      ) : null}

      {/* One polite announcement when a node is done; a failure is announced by its alert. */}
      <span role="status" className="sr-only">
        {item.state === "done" ? t("pulls.node.announce.done", { node: item.name }) : ""}
      </span>

      {open ? (
        <div id={panelId}>
          {layers.length === 0 ? (
            <p className="text-sm text-muted-foreground">{t("pulls.node.noLayers")}</p>
          ) : (
            <Table>
              <TableCaption className="sr-only">
                {t("pulls.node.layerTable", { node: item.name })}
              </TableCaption>
              <TableHeader>
                <TableRow>
                  <TableHead>{t("pulls.node.columns.layer")}</TableHead>
                  <TableHead>{t("pulls.node.columns.status")}</TableHead>
                  <TableHead>{t("pulls.node.columns.progress")}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {layers.map((l) => (
                  <TableRow key={l.id}>
                    <TableCell className="font-mono text-xs break-all">{l.id}</TableCell>
                    <TableCell className="break-words">{l.status}</TableCell>
                    <TableCell className="tabular-nums">
                      {l.total > 0
                        ? t("pulls.node.bytes", {
                            done: formatBytes(l.current),
                            total: formatBytes(l.total),
                          })
                        : t("dashboard.time.missing")}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </div>
      ) : (
        <span id={panelId} hidden />
      )}
    </li>
  );
}
