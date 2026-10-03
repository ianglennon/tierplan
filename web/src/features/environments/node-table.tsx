// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Copy, Ellipsis } from "lucide-react";
import { useState } from "react";

import { type AgentItem, agentsQuery } from "@/api/agents";
import { type EnvironmentItem, environmentsQuery } from "@/api/environments";
import { CERT_WARN_DAYS, CertExpiry } from "@/components/cert-expiry";
import { copyText } from "@/components/copyable-code";
import { RelativeTime, useNow } from "@/components/relative-time";
import { StatusDot } from "@/components/status-dot";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Skeleton } from "@/components/ui/skeleton";
import { TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { useEnvHealthEvents } from "@/features/environments/health-stream";
import { RevokeDialog, UnblockDialog } from "@/features/environments/revoke-dialog";
import { usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { formatClockOffset } from "@/lib/format";
import { toastError, toastSuccess } from "@/lib/toast";
import { cn } from "@/lib/utils";

const DAY = 86_400_000;

// A clock further than this from the server's is flagged (UI-SPEC Clock cell).
const CLOCK_WARN_SECONDS = 30;

// How many hex characters of the certificate fingerprint the cell shows.
const SHORT_FINGERPRINT = 12;

// Skeleton rows while the first read is out (UI-SPEC: 36px rows).
const SKELETON_ROWS = 6;

// A revoked or blocked row is a problem whatever else it says.
const isRevoked = (a: AgentItem) => a.status === "revoked" || a.blocked;

// Whether an offset means anything: an agent that has not connected since the
// server started reports 0, which is not a measurement.
const clockKnown = (a: AgentItem) => a.connected || a.clockOffsetSeconds !== 0;

function clockOff(a: AgentItem): boolean {
  return clockKnown(a) && Math.abs(Math.trunc(a.clockOffsetSeconds)) > CLOCK_WARN_SECONDS;
}

function certSoon(a: AgentItem, now: number): boolean {
  const end = Date.parse(a.certNotAfter);
  return !Number.isNaN(end) && end - now < CERT_WARN_DAYS * DAY;
}

// How bad a row is, worst first (UI-SPEC "problems first"): revoked or blocked,
// then not connected, then a warning (certificate under 7 days or a clock above
// 30 seconds), then the rest.
function severity(a: AgentItem, now: number): number {
  if (isRevoked(a)) return 0;
  if (!a.connected) return 1;
  if (certSoon(a, now) || clockOff(a)) return 2;
  return 3;
}

// sortAgents orders the rows problems first, then by node name. The server
// sends them in much the same order, but a row that changed from a stream
// event should land where it belongs before the next read.
export function sortAgents(agents: AgentItem[], now: number): AgentItem[] {
  return [...agents].sort(
    (a, b) =>
      severity(a, now) - severity(b, now) ||
      a.node.localeCompare(b.node, undefined, { sensitivity: "base" }) ||
      a.id.localeCompare(b.id),
  );
}

// Truncated renders text on one line with an ellipsis and the whole value in a
// tooltip that opens on hover and on focus. What an agent reported is only ever
// a React string here, never markup (T-02-59).
function Truncated({
  value,
  mono,
  className,
}: {
  value: string;
  mono?: boolean;
  className?: string;
}) {
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <span
          // biome-ignore lint/a11y/noNoninteractiveTabindex: the tooltip must open on keyboard focus (UI-SPEC long text)
          tabIndex={0}
          className={cn(
            "block min-w-0 truncate rounded-sm focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none",
            mono && "font-mono text-xs",
            className,
          )}
        >
          {value}
        </span>
      </TooltipTrigger>
      <TooltipContent className="max-w-sm font-mono break-all">{value}</TooltipContent>
    </Tooltip>
  );
}

function Dash() {
  const t = useT();
  return <span className="text-muted-foreground">{t("envsettings.nodes.missing")}</span>;
}

function Fingerprint({ agent }: { agent: AgentItem }) {
  const t = useT();
  const full = agent.certFingerprint;
  return (
    <span className="inline-flex items-center gap-1">
      <Tooltip>
        <TooltipTrigger asChild>
          <span
            // biome-ignore lint/a11y/noNoninteractiveTabindex: the tooltip must open on keyboard focus (UI-SPEC long text)
            tabIndex={0}
            className="rounded-sm font-mono text-xs focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
          >
            {full.slice(0, SHORT_FINGERPRINT)}
          </span>
        </TooltipTrigger>
        <TooltipContent className="max-w-sm font-mono break-all">{full}</TooltipContent>
      </Tooltip>
      {/* A tooltip's content cannot take keyboard focus, so the copy button sits
          beside the short value and the tooltip shows the whole one. */}
      <Button
        type="button"
        variant="ghost"
        size="icon-xs"
        className="max-md:size-11"
        aria-label={t("envsettings.nodes.copyFingerprint", { node: agent.node })}
        onClick={async () => {
          if (await copyText(full)) toastSuccess(t("envsettings.nodes.fingerprintCopied"));
          else toastError(t("env.copy.failed"));
        }}
      >
        <Copy aria-hidden="true" />
      </Button>
    </span>
  );
}

function ClockCell({ agent }: { agent: AgentItem }) {
  const t = useT();
  if (!clockKnown(agent)) return <Dash />;
  const off = clockOff(agent);
  return (
    <span className="inline-flex flex-wrap items-baseline gap-x-2 tabular-nums">
      <span className={cn(off && "text-status-warning-text")}>
        {formatClockOffset(agent.clockOffsetSeconds)}
      </span>
      {off ? (
        <span className="text-xs text-status-warning-text">{t("envsettings.nodes.clockOff")}</span>
      ) : null}
    </span>
  );
}

interface RowProps {
  agent: AgentItem;
  // The environment's status issues name a node whose certificate is not renewing.
  renewalFailing: boolean;
  canRevoke: boolean;
  onRevoke: (agent: AgentItem) => void;
  onUnblock: (agent: AgentItem) => void;
}

function NodeRow({ agent, renewalFailing, canRevoke, onRevoke, onUnblock }: RowProps) {
  const t = useT();
  const revoked = isRevoked(agent);
  const canRevokeThis = canRevoke && agent.status === "active";
  const canUnblockThis = canRevoke && agent.blocked;
  return (
    <TableRow className="h-9">
      <TableCell className="max-w-56">
        <div className="flex items-center gap-2">
          <Truncated value={agent.node} mono />
          {agent.status === "revoked" ? (
            <Badge variant="outline" className="shrink-0 text-status-offline-text">
              {t("envsettings.nodes.badges.revoked")}
            </Badge>
          ) : null}
          {agent.blocked ? (
            <Badge variant="outline" className="shrink-0 text-status-offline-text">
              {t("envsettings.nodes.badges.blocked")}
            </Badge>
          ) : null}
          {agent.readOnly ? (
            <Tooltip>
              <TooltipTrigger asChild>
                <Badge
                  variant="secondary"
                  // Focusable so the tooltip opens on keyboard focus (D-28).
                  tabIndex={0}
                  className="shrink-0"
                >
                  {t("envsettings.nodes.badges.readOnly")}
                </Badge>
              </TooltipTrigger>
              <TooltipContent>{t("envsettings.nodes.badges.readOnlyTip")}</TooltipContent>
            </Tooltip>
          ) : null}
        </div>
      </TableCell>
      <TableCell>{t(`envsettings.nodes.roles.${agent.role}`)}</TableCell>
      <TableCell>
        {revoked ? (
          <Dash />
        ) : (
          <StatusDot
            state={agent.connected ? "online" : "offline"}
            label={t(
              `envsettings.nodes.agentState.${agent.connected ? "connected" : "disconnected"}`,
            )}
          />
        )}
      </TableCell>
      <TableCell className="max-w-32">
        {agent.agentVersion ? <Truncated value={agent.agentVersion} mono /> : <Dash />}
      </TableCell>
      <TableCell className="max-w-32">
        {agent.engineVersion ? <Truncated value={agent.engineVersion} mono /> : <Dash />}
      </TableCell>
      <TableCell>
        {revoked ? (
          <Dash />
        ) : (
          <div className="flex flex-col gap-0.5">
            <Fingerprint agent={agent} />
            <CertExpiry notAfter={agent.certNotAfter} renewalFailing={renewalFailing} />
          </div>
        )}
      </TableCell>
      <TableCell>{revoked ? <Dash /> : <ClockCell agent={agent} />}</TableCell>
      <TableCell>
        <RelativeTime value={agent.lastSeen} />
      </TableCell>
      <TableCell className="w-12 text-end">
        {canRevokeThis || canUnblockThis ? (
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button
                type="button"
                variant="ghost"
                size="icon"
                className="max-md:size-11"
                aria-label={t("envsettings.nodes.actionsFor", { node: agent.node })}
              >
                <Ellipsis aria-hidden="true" />
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end">
              {canRevokeThis ? (
                <DropdownMenuItem
                  variant="destructive"
                  className="max-md:min-h-11"
                  onSelect={() => onRevoke(agent)}
                >
                  {t("envsettings.nodes.revokeNode")}
                </DropdownMenuItem>
              ) : null}
              {canUnblockThis ? (
                <DropdownMenuItem className="max-md:min-h-11" onSelect={() => onUnblock(agent)}>
                  {t("envsettings.nodes.unblockNode")}
                </DropdownMenuItem>
              ) : null}
            </DropdownMenuContent>
          </DropdownMenu>
        ) : null}
      </TableCell>
    </TableRow>
  );
}

// LocalRow is the one row of a local-socket environment: the host Bayplan runs
// on has no agent, no certificate and no clock, and nothing to revoke.
function LocalRow({ env }: { env: EnvironmentItem }) {
  const t = useT();
  return (
    <TableRow className="h-9">
      <TableCell className="max-w-56">
        <Truncated value={env.name} mono />
      </TableCell>
      <TableCell>{t("envsettings.nodes.roles.localSocket")}</TableCell>
      <TableCell>
        <Dash />
      </TableCell>
      <TableCell>
        <Dash />
      </TableCell>
      <TableCell className="max-w-32">
        {env.engine.version ? <Truncated value={env.engine.version} mono /> : <Dash />}
      </TableCell>
      <TableCell>
        <Dash />
      </TableCell>
      <TableCell>
        <Dash />
      </TableCell>
      <TableCell>
        <RelativeTime value={env.lastSeen ?? undefined} />
      </TableCell>
      <TableCell className="w-12" />
    </TableRow>
  );
}

interface NodeTableProps {
  envId: string;
  // Called from the toast after a revoke: opens the install dialog in rotate mode.
  onRotateToken?: () => void;
}

// NodeTable is "Nodes and agents" (D-04, D-08, D-45): every node's agent with
// its certificate, clock and last seen, problems first, with Revoke node and
// Unblock node for someone who holds environments.revoke. It scrolls inside its
// card, vertically for a swarm of fifty nodes and sideways on a phone, and the
// header stays put. A local-socket environment shows its one row and no actions.
export function NodeTable({ envId, onRotateToken }: NodeTableProps) {
  const t = useT();
  const queryClient = useQueryClient();
  const now = useNow();
  const canRevoke = usePermission("environments.revoke", envId);
  const environments = useQuery(environmentsQuery);
  const env = environments.data?.items?.find((e) => e.id === envId);
  const isLocal = env?.kind === "local";
  const agents = useQuery({ ...agentsQuery(envId), enabled: !isLocal });
  const [revoking, setRevoking] = useState<AgentItem | null>(null);
  const [unblocking, setUnblocking] = useState<AgentItem | null>(null);

  // An agent that connects or goes away, or a status change, makes the list stale.
  useEnvHealthEvents(envId, () => {
    void queryClient.invalidateQueries({ queryKey: agentsQuery(envId).queryKey });
  });

  const loading = environments.isPending || (!isLocal && agents.isPending);
  const failed = !loading && !isLocal && agents.isError;
  const list = isLocal ? [] : (agents.data ?? []);

  if (failed) {
    return (
      <Alert variant="destructive">
        <AlertDescription className="flex flex-wrap items-center gap-3">
          <span>{t("envsettings.nodes.loadFailed")}</span>
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="max-md:h-11"
            disabled={agents.isFetching}
            onClick={() => void agents.refetch()}
          >
            {t("envsettings.nodes.retry")}
          </Button>
        </AlertDescription>
      </Alert>
    );
  }

  if (!loading && !isLocal && list.length === 0) {
    return <p className="text-sm text-muted-foreground">{t("envsettings.nodes.empty")}</p>;
  }

  // Which nodes' certificates the server says are not renewing.
  const notRenewing = new Set(
    (env?.status.issues ?? []).filter((i) => i.code === "cert_expiring").map((i) => i.node),
  );
  const sorted = sortAgents(list, now);

  return (
    <>
      <span className="sr-only" role="status">
        {loading ? t("envsettings.nodes.loading") : ""}
      </span>
      {/* The card scrolls, not the page: fifty nodes stay inside it, with the
          header held at the top. */}
      <div data-scroll className="max-h-[28rem] overflow-auto rounded-md border">
        <table
          aria-label={t("envsettings.nodes.tableLabel")}
          aria-busy={loading || undefined}
          className="w-full min-w-[56rem] caption-bottom text-sm"
        >
          <TableHeader className="sticky top-0 z-10 bg-muted">
            <TableRow className="h-12">
              {(
                [
                  "node",
                  "role",
                  "agent",
                  "agentVersion",
                  "engine",
                  "certificate",
                  "clock",
                  "lastSeen",
                ] as const
              ).map((key) => (
                <TableHead key={key} className="text-xs font-semibold">
                  {t(`envsettings.nodes.columns.${key}`)}
                </TableHead>
              ))}
              <TableHead className="w-12 text-xs font-semibold">
                <span className="sr-only">{t("envsettings.nodes.columns.actions")}</span>
              </TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {loading
              ? Array.from({ length: SKELETON_ROWS }, (_, i) => (
                  // biome-ignore lint/suspicious/noArrayIndexKey: placeholder rows have no identity
                  <TableRow key={i} className="h-9">
                    <TableCell colSpan={9}>
                      <Skeleton className="h-4 w-full" />
                    </TableCell>
                  </TableRow>
                ))
              : null}
            {!loading && isLocal && env ? <LocalRow env={env} /> : null}
            {!loading
              ? sorted.map((agent) => (
                  <NodeRow
                    key={agent.id}
                    agent={agent}
                    renewalFailing={notRenewing.has(agent.node)}
                    canRevoke={canRevoke}
                    onRevoke={setRevoking}
                    onUnblock={setUnblocking}
                  />
                ))
              : null}
          </TableBody>
        </table>
      </div>
      <RevokeDialog
        envId={envId}
        agent={revoking}
        onClose={() => setRevoking(null)}
        onRotateToken={onRotateToken}
      />
      <UnblockDialog envId={envId} agent={unblocking} onClose={() => setUnblocking(null)} />
    </>
  );
}
