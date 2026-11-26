// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useState } from "react";
import { Trans } from "react-i18next";

import type { HealthState } from "@/api/environments";
import { ingressStatusQuery } from "@/api/ingress";
import {
  type IngressNode,
  type IngressVIPCandidate,
  ingressNodesQuery,
  ingressVIPQuery,
} from "@/api/ingress-nodes";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { RelativeTime } from "@/components/relative-time";
import { StatusDot } from "@/components/status-dot";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from "@/components/ui/empty";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { IngressOffEmpty } from "@/features/ingress/ingress-off-empty";
import { RevokeNodeDialog } from "@/features/ingress/revoke-node-dialog";
import { VIPMoves } from "@/features/ingress/vip-moves";
import { VIPStatusCard } from "@/features/ingress/vip-status-card";
import { useEnvIsSwarm } from "@/features/resources/swarm-lists";
import { type Needs, useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";

// How much of a bundle ID is shown: the IDs are content hashes, so a prefix tells two apart (A-05).
const SHORT = 8;

function short(id: string): string {
  return id.slice(0, SHORT);
}

// enrolment is the dot and the key of the words for a node's enrolment (UI-SPEC S4): a revoked node is revoked, an
// active one is connected or, when it has no live session, not connected.
function enrolment(n: IngressNode): { state: HealthState; key: string } {
  if (n.status === "revoked")
    return { state: "revoked", key: "ingressPage.nodes.enrolment.revoked" };
  return n.connected
    ? { state: "online", key: "ingressPage.nodes.enrolment.connected" }
    : { state: "degraded", key: "ingressPage.nodes.enrolment.notConnected" };
}

// health is what the node last said about its proxy; before it has said anything, it is not reported.
function health(n: IngressNode): { state: HealthState; key: string } {
  switch (n.health) {
    case "ok":
      return { state: "online", key: "ingressPage.nodes.health.ok" };
    case "unhealthy":
      return { state: "offline", key: "ingressPage.nodes.health.unhealthy" };
    default:
      return { state: "unknown", key: "ingressPage.nodes.health.none" };
  }
}

// sync is how the node stands against the bundle it should hold: pending is expected and not a fault, so it is the
// hollow ring, never red.
function sync(n: IngressNode): { state: HealthState; key: string } {
  switch (n.sync) {
    case "current":
      return { state: "online", key: "ingressPage.nodes.sync.current" };
    case "pending":
      return { state: "pending", key: "ingressPage.nodes.sync.pending" };
    case "incompatible":
      return { state: "degraded", key: "ingressPage.nodes.sync.incompatible" };
    default:
      return { state: "unknown", key: "ingressPage.nodes.sync.unknown" };
  }
}

// candidateState is the dot and the key of the words for what a candidate's keepalived last said (UI-SPEC S4): holding the
// address and standing by are both healthy, a fault is a warning, a stopped one is not a fault, and one that has said
// nothing is not reporting.
function candidateState(c: IngressVIPCandidate): { state: HealthState; key: string } {
  switch (c.vrrpState) {
    case "MASTER":
      return { state: "online", key: "ingressPage.nodes.vip.state.MASTER" };
    case "BACKUP":
      return { state: "online", key: "ingressPage.nodes.vip.state.BACKUP" };
    case "FAULT":
      return { state: "degraded", key: "ingressPage.nodes.vip.state.FAULT" };
    case "STOP":
      return { state: "unknown", key: "ingressPage.nodes.vip.state.STOP" };
    default:
      return { state: "unknown", key: "ingressPage.nodes.vip.state.none" };
  }
}

// NodesTab is the Nodes tab of the Ingress page (UIX-04, UI-SPEC S4, D-13): every ingress node of the environment
// with its enrolment, supervisor version, proxy health, the bundle it applied against the one it should hold, and
// when it was last heard from. Bundle IDs are unordered content hashes, so the table never says how far behind a node
// is (A-05).
export function NodesTab({ envId }: { envId: string }) {
  const t = useT();
  const query = useQuery(ingressNodesQuery(envId));
  const status = useQuery(ingressStatusQuery(envId));
  // The virtual IP belongs to a swarm only (Pitfall 14): a standalone environment never asks for it. A swarm with none
  // answers null, which is the "off" line, not an error.
  const swarm = useEnvIsSwarm(envId);
  const vipQuery = useQuery(ingressVIPQuery(envId, swarm));
  const vip = vipQuery.data ?? null;
  const manage = useNeeds("ingress.manage", envId).allowed;
  const revokeNeeds = useNeeds("environments.revoke", envId);
  const [revoking, setRevoking] = useState<IngressNode | null>(null);

  if (query.isPending) return <Loading />;
  if (query.isError) {
    return (
      <Alert variant="destructive">
        <AlertDescription className="flex flex-wrap items-center gap-3">
          <span>{t("ingressPage.nodes.error")}</span>
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="max-md:h-11"
            disabled={query.isFetching}
            onClick={() => void query.refetch()}
          >
            {t("ingressPage.nodes.retry")}
          </Button>
        </AlertDescription>
      </Alert>
    );
  }

  const items = query.data.items ?? [];
  if (items.length === 0) {
    // A node can outlive ingress being turned off, so the off state is only for a tab with nothing in it.
    if (status.isPending) return <Loading />;
    if (status.data?.enabled === false) return <IngressOffEmpty envId={envId} />;
    return (
      <Empty>
        <EmptyHeader>
          <EmptyTitle role="heading" aria-level={3}>
            {t("ingressPage.nodes.empty.title")}
          </EmptyTitle>
          <EmptyDescription>{t("ingressPage.nodes.empty.body")}</EmptyDescription>
        </EmptyHeader>
      </Empty>
    );
  }

  const currentBundleId = query.data.currentBundleId;
  // The Virtual IP column is a join of the candidates onto the ingress nodes by swarm node ID, not a second list.
  const candidates = vip ? new Map((vip.candidates ?? []).map((c) => [c.nodeId, c])) : null;
  return (
    <div className="flex flex-col gap-4">
      {swarm && vipQuery.isSuccess && vip === null ? (
        <p className="text-sm text-muted-foreground">
          {manage ? (
            <Trans
              i18nKey="ingressPage.nodes.vip.offLinked"
              ns="ingressPage"
              components={{
                settings: (
                  <Link
                    to="/env/$envId/ingress/settings"
                    params={{ envId }}
                    className="rounded-sm text-accent-ink hover:underline"
                  />
                ),
              }}
            />
          ) : (
            t("ingressPage.nodes.vip.off")
          )}
        </p>
      ) : null}
      {vip ? <VIPStatusCard vip={vip} /> : null}
      <Table aria-label={t("ingressPage.nodes.label")}>
        <TableHeader>
          <TableRow>
            <TableHead scope="col">{t("ingressPage.nodes.columns.node")}</TableHead>
            <TableHead scope="col">{t("ingressPage.nodes.columns.enrolment")}</TableHead>
            <TableHead scope="col">{t("ingressPage.nodes.columns.supervisor")}</TableHead>
            <TableHead scope="col">{t("ingressPage.nodes.columns.health")}</TableHead>
            <TableHead scope="col">{t("ingressPage.nodes.columns.bundle")}</TableHead>
            <TableHead scope="col">{t("ingressPage.nodes.columns.lastSeen")}</TableHead>
            {candidates ? (
              <TableHead scope="col">{t("ingressPage.nodes.columns.vip")}</TableHead>
            ) : null}
            <TableHead scope="col">
              <span className="sr-only">{t("ingressPage.nodes.columns.actions")}</span>
            </TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {items.map((n) => (
            <NodeRow
              key={n.id}
              envId={envId}
              node={n}
              currentBundleId={currentBundleId}
              candidates={candidates}
              manage={manage}
              revokeNeeds={revokeNeeds}
              onRevoke={setRevoking}
            />
          ))}
        </TableBody>
      </Table>
      {vip ? <VIPMoves envId={envId} /> : null}
      <RevokeNodeDialog envId={envId} node={revoking} onClose={() => setRevoking(null)} />
    </div>
  );
}

function Loading() {
  const t = useT();
  return (
    <div>
      <span role="status" className="sr-only">
        {t("ingressPage.nodes.loading")}
      </span>
      <Skeleton className="h-48 w-full" aria-hidden="true" />
    </div>
  );
}

function NodeRow({
  envId,
  node: n,
  currentBundleId,
  candidates,
  manage,
  revokeNeeds,
  onRevoke,
}: {
  envId: string;
  node: IngressNode;
  currentBundleId: string;
  // The virtual IP's candidates by swarm node ID; null when the environment has no virtual IP, which leaves out its column.
  candidates: Map<string, IngressVIPCandidate> | null;
  manage: boolean;
  revokeNeeds: Needs;
  onRevoke: (node: IngressNode) => void;
}) {
  const t = useT();
  const revoked = n.status === "revoked";
  const enrol = enrolment(n);
  const proxy = health(n);
  const bundle = sync(n);
  const candidate = candidates && n.swarmNodeId !== "" ? candidates.get(n.swarmNodeId) : undefined;
  const vrrp = candidate ? candidateState(candidate) : null;
  return (
    <TableRow>
      <TableCell className="align-top whitespace-normal">
        {n.swarmNodeId !== "" ? (
          <Link
            to="/env/$envId/nodes/$nodeId"
            params={{ envId, nodeId: n.swarmNodeId }}
            className="text-sm font-medium break-all underline-offset-4 hover:underline"
          >
            {n.name}
          </Link>
        ) : (
          <span className="text-sm font-medium break-all">{n.name}</span>
        )}
        {n.createdAt ? (
          <p className="text-xs text-muted-foreground">
            <Trans
              i18nKey="ingressPage.nodes.enrolled"
              ns="ingressPage"
              components={{ time: <RelativeTime value={n.createdAt} /> }}
            />
          </p>
        ) : null}
      </TableCell>
      <TableCell className="align-top whitespace-normal">
        <StatusDot state={enrol.state} label={t(enrol.key)} />
        {revoked && n.revokedAt ? (
          <p className="text-xs text-muted-foreground">
            <Trans
              i18nKey="ingressPage.nodes.revokedAt"
              ns="ingressPage"
              components={{ time: <RelativeTime value={n.revokedAt} /> }}
            />
          </p>
        ) : null}
        {revoked && manage ? (
          <div className="flex flex-col gap-0.5">
            {n.revokeReason ? <p className="text-xs break-words">{n.revokeReason}</p> : null}
            <p className="text-xs text-muted-foreground">{t("ingressPage.nodes.tokenMounted")}</p>
          </div>
        ) : null}
      </TableCell>
      <TableCell className="align-top whitespace-normal">
        <span className="font-mono text-xs">{n.version}</span>
        {n.incompatible ? (
          <p className="text-xs text-status-warning-text">{t("ingressPage.nodes.incompatible")}</p>
        ) : null}
      </TableCell>
      <TableCell className="align-top whitespace-normal">
        <StatusDot state={proxy.state} label={t(proxy.key)} />
      </TableCell>
      <TableCell className="align-top whitespace-normal">
        <StatusDot state={bundle.state} label={t(bundle.key)} />
        <p className="font-mono text-xs text-muted-foreground">
          {n.appliedBundleId !== ""
            ? t("ingressPage.nodes.applied", { id: short(n.appliedBundleId) })
            : t("ingressPage.nodes.appliedNone")}
        </p>
        {!revoked && n.sync !== "current" && currentBundleId !== "" ? (
          <p className="font-mono text-xs text-muted-foreground">
            {t("ingressPage.nodes.currentBundle", { id: short(currentBundleId) })}
          </p>
        ) : null}
        {n.rejectedBundleId !== "" ? (
          <p className="font-mono text-xs break-words text-status-warning-text">
            {t("ingressPage.nodes.refused", {
              id: short(n.rejectedBundleId),
              reason: n.rejectedReason,
            })}
          </p>
        ) : null}
      </TableCell>
      <TableCell className="align-top text-sm whitespace-normal">
        {n.lastSeenAt ? <RelativeTime value={n.lastSeenAt} /> : t("ingressPage.nodes.never")}
      </TableCell>
      {candidates ? (
        <TableCell className="align-top whitespace-normal">
          {candidate && vrrp ? (
            <>
              <StatusDot state={vrrp.state} label={t(vrrp.key)} />
              {candidate.vrrpState === "FAULT" && candidate.lastFault !== "" ? (
                <p className="text-xs break-words text-status-warning-text">
                  {candidate.lastFault}
                </p>
              ) : null}
            </>
          ) : (
            <span className="text-sm text-muted-foreground">
              {t("ingressPage.nodes.vip.notCandidate")}
            </span>
          )}
        </TableCell>
      ) : null}
      <TableCell className="align-top">
        {revoked ? null : (
          <NeedsTooltip reason={revokeNeeds.reason}>
            <Button
              type="button"
              variant="ghost"
              size="sm"
              className="max-md:h-11"
              aria-label={t("ingressPage.nodes.revoke.buttonFor", { name: n.name })}
              onClick={() => onRevoke(n)}
            >
              {t("ingressPage.nodes.revoke.button")}
            </Button>
          </NeedsTooltip>
        )}
      </TableCell>
    </TableRow>
  );
}
