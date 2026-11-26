// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { ChevronDown } from "lucide-react";
import { Fragment, useState } from "react";
import { Trans } from "react-i18next";

import type { HealthState } from "@/api/environments";

import { type IngressRoute, ingressRoutesQuery, type RouteCondition } from "@/api/ingress";
import { ingressCertificatesQuery } from "@/api/ingress-certs";
import { StatusDot } from "@/components/status-dot";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from "@/components/ui/empty";
import { Input } from "@/components/ui/input";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group";
import { IngressOffEmpty } from "@/features/ingress/ingress-off-empty";
import { useEnvIsSwarm } from "@/features/resources/swarm-lists";
import { routeHeadline } from "@/features/stacks/route-state";
import { useNeeds } from "@/hooks/use-needs";
import { keyExists, useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

// At most this many rows show until the person asks for the rest (UI-SPEC S2, the stack Routes card's pattern).
const SHOWN = 10;

// The swarm service that runs the proxy, and the container of the standalone host's proxy: its compose project
// tierplan-ingress, service proxy, first replica (internal/ingress/objects.go, disable_standalone.go).
const PROXY_SERVICE = "tierplan-ingress";
const PROXY_CONTAINER = "tierplan-ingress-proxy-1";

// routeText is the hostname and the path joined, as a person types it; a route for the whole host is the hostname
// alone.
function routeText(route: IngressRoute): string {
  return route.path === "" || route.path === "/"
    ? route.hostname
    : `${route.hostname}${route.path}`;
}

// readyCount is how many of a route's conditions hold. A condition the server could not read is not ready.
function readyCount(route: IngressRoute): number {
  return (route.conditions ?? []).filter((c) => c.state === "ready").length;
}

// conditionDot is the dot of one condition (UI-SPEC Color, condition state). An unread condition is grey and is never
// drawn as ready. A stopped stack's backend is grey, because it is stopped on purpose, and any other backend that is
// not ready is a fault, so red.
function conditionDot(c: RouteCondition): HealthState {
  switch (c.state) {
    case "ready":
      return "online";
    case "unknown":
      return "unknown";
    case "not_ready":
      if (c.code !== "backend_healthy") return "degraded";
      return c.reason === "stack_stopped" ? "unknown" : "offline";
  }
}

// reasonOf is the one sentence under a condition. A ready condition has one only for applied; a known code is worded
// here, and a code this build does not know shows the server's message as sent, as text.
function reasonOf(
  c: RouteCondition,
  route: IngressRoute,
  t: (key: string, options?: Record<string, unknown>) => string,
): string {
  if (c.state === "ready") {
    return c.code === "applied" && c.applied
      ? t("ingressPage.reasons.appliedReady", { applied: c.applied.nodes, total: c.applied.total })
      : "";
  }
  const key = `ingressPage.reasons.${c.reason}`;
  if (c.reason !== "" && keyExists(key)) {
    return t(key, {
      service: route.service,
      applied: c.applied?.nodes ?? 0,
      total: c.applied?.total ?? 0,
    });
  }
  return c.message;
}

// detailOf is the one line under a route's headline: why it is in that state, in the words the stack page uses. A
// route whose state the engine could not read says why in the server's reason for its first unread condition, and
// is not an error: the tab still draws.
function detailOf(
  r: IngressRoute,
  standalone: boolean,
  t: (key: string, options?: Record<string, unknown>) => string,
): string {
  switch (r.state) {
    case "pending":
      return t(
        standalone
          ? "stackIngress.routes.state.pending.detailStandalone"
          : "stackIngress.routes.state.pending.detail",
      );
    case "not_attached":
    case "ingress_off":
      return t(`stackIngress.routes.state.${r.state}.detail`);
    case "waiting_certificate": {
      if (r.reason === "") return "";
      const key = `stackIngress.routes.reason.${r.reason}`;
      return keyExists(key) ? t(key) : r.reason;
    }
    case "backend_not_running":
      if (r.stopped) return t("stackIngress.routes.state.backend_not_running.stopped");
      return t(
        standalone
          ? "stackIngress.routes.state.backend_not_running.noContainer"
          : "stackIngress.routes.state.backend_not_running.noTask",
        { service: r.service },
      );
    case "unknown": {
      const unread = (r.conditions ?? []).find((c) => c.state === "unknown");
      return unread ? reasonOf(unread, r, t) : t("ingressPage.reasons.engine_unreadable");
    }
    default:
      return "";
  }
}

// isRoute says whether a row is a route a stack serves and so has a state and five conditions. A TierPlan route has
// no state and is not drawn (Phase 28 draws it).
function isRoute(r: IngressRoute): boolean {
  return r.kind === "route" && r.state !== "";
}

// isDrawn says whether the tab draws a row: a route, a name a stack holds with no route to serve (D-08), and a
// reserved name. A claim TierPlan itself holds is not a stack's and is not drawn.
function isDrawn(r: IngressRoute): boolean {
  return isRoute(r) || (r.kind === "claim" && r.ownerKind === "stack") || r.kind === "reserved";
}

// needsAttention says whether a row is one a person should look at (D-25): a route that is not serving, whose stack
// is not stopped on purpose and that is not simply off. A held name and a reserved name are not routes, so they
// are not faults.
function needsAttention(r: IngressRoute): boolean {
  return isRoute(r) && r.state !== "serving" && r.state !== "ingress_off" && !r.stopped;
}

// matches is the filter (D-25): the text appears, in any case, in the hostname, the path, the stack's name or the
// service's name.
function matches(r: IngressRoute, needle: string): boolean {
  if (needle === "") return true;
  return [r.hostname, r.path, r.stackName, r.service].some((v) => v.toLowerCase().includes(needle));
}

type View = "all" | "attention";

// RoutesTab is the Routes tab of the Ingress page (UIX-02, UI-SPEC S2): each route of the environment with the
// stack and service behind it, its headline state drawn through the map the stack page uses, and how many of its
// five conditions hold, then the names a stack holds with no route and the names reserved (D-08, U-16), so one scan
// answers why a hostname is not working. A TierPlan route carries no state and is not drawn here.
// Every value renders as text: no hostname is a link (U-08).
export function RoutesTab({ envId }: { envId: string }) {
  const t = useT();
  const query = useQuery(ingressRoutesQuery(envId));
  // The hostnames that have a certificate row, to know which routes can link to theirs (D-11). A read that failed or
  // has not come back is no certificate known, so the cell says nothing rather than link to an empty filter.
  const certs = useQuery(ingressCertificatesQuery(envId));
  const certified = new Set((certs.data?.items ?? []).map((c) => c.hostname.toLowerCase()));
  // The routes whose conditions are open, by hostname, path and service. It is local to the tab, so it survives a
  // refetch, and nothing opens by itself.
  const [open, setOpen] = useState<ReadonlySet<string>>(new Set());
  const [filter, setFilter] = useState("");
  const [view, setView] = useState<View>("all");
  const [all, setAll] = useState(false);
  const swarm = useEnvIsSwarm(envId);
  // The proxy's logs are a link to the log view the page already has, and only for someone who may read it there.
  const logsRead = useNeeds(swarm ? "services.read" : "containers.read", envId);
  const toggle = (key: string) =>
    setOpen((prev) => {
      const next = new Set(prev);
      if (!next.delete(key)) next.add(key);
      return next;
    });

  if (query.isPending) {
    return (
      <div>
        <span role="status" className="sr-only">
          {t("ingressPage.routes.loading")}
        </span>
        <Skeleton className="h-48 w-full" aria-hidden="true" />
      </div>
    );
  }
  if (query.isError) {
    return (
      <Alert variant="destructive">
        <AlertDescription className="flex flex-wrap items-center gap-3">
          <span>{t("ingressPage.routes.error")}</span>
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="max-md:h-11"
            disabled={query.isFetching}
            onClick={() => void query.refetch()}
          >
            {t("ingressPage.routes.retry")}
          </Button>
        </AlertDescription>
      </Alert>
    );
  }

  if (query.data.enabled === false) return <IngressOffEmpty envId={envId} />;

  const everything = (query.data.routes ?? []).filter(isDrawn);
  const routes = everything.filter(isRoute);
  const needle = filter.trim().toLowerCase();
  const rows = everything.filter(
    (r) => matches(r, needle) && (view === "all" || needsAttention(r)),
  );
  const shown = all ? rows : rows.slice(0, SHOWN);

  return (
    <div className="flex flex-col gap-3">
      <div className="flex flex-wrap items-center gap-2">
        {everything.length > 0 ? (
          <>
            <Input
              type="text"
              aria-label={t("ingressPage.routes.filter")}
              placeholder={t("ingressPage.routes.filterPlaceholder")}
              value={filter}
              onChange={(e) => setFilter(e.target.value)}
              className="max-w-sm max-md:h-11"
            />
            <ToggleGroup
              type="single"
              role="group"
              aria-label={t("ingressPage.routes.view.label")}
              variant="outline"
              size="sm"
              spacing={2}
              value={view}
              // Pressing the one that is on would turn it off: there is always a view.
              onValueChange={(v) => {
                if (v) setView(v as View);
              }}
            >
              {(["all", "attention"] as const).map((v) => (
                <ToggleGroupItem
                  key={v}
                  value={v}
                  className="aria-checked:border-ring aria-checked:text-ring max-md:h-11"
                >
                  {t(`ingressPage.routes.view.${v}`)}
                </ToggleGroupItem>
              ))}
            </ToggleGroup>
          </>
        ) : null}
        {logsRead.allowed ? (
          <Link
            {...(swarm
              ? {
                  to: "/env/$envId/services/$serviceId/logs" as const,
                  params: { envId, serviceId: PROXY_SERVICE },
                }
              : {
                  to: "/env/$envId/containers/$containerId/logs" as const,
                  params: { envId, containerId: PROXY_CONTAINER },
                })}
            className="ms-auto rounded-sm text-sm text-accent-ink hover:underline max-md:py-3"
          >
            {t("ingressPage.routes.proxyLogs")}
          </Link>
        ) : null}
      </div>
      <p className="text-sm text-muted-foreground">
        {t("ingressPage.routes.summary", {
          count: routes.length,
          serving: routes.filter((r) => r.state === "serving").length,
          attention: routes.filter(needsAttention).length,
          stopped: routes.filter((r) => r.stopped).length,
        })}
      </p>
      {everything.length === 0 ? (
        <Empty>
          <EmptyHeader>
            <EmptyTitle role="heading" aria-level={3}>
              {t("ingressPage.routes.empty.title")}
            </EmptyTitle>
            <EmptyDescription>{t("ingressPage.routes.empty.body")}</EmptyDescription>
          </EmptyHeader>
        </Empty>
      ) : (
        <>
          <Table aria-label={t("ingressPage.routes.label")}>
            <TableHeader>
              <TableRow>
                <TableHead scope="col">{t("ingressPage.routes.columns.route")}</TableHead>
                <TableHead scope="col">{t("ingressPage.routes.columns.stack")}</TableHead>
                <TableHead scope="col">{t("ingressPage.routes.columns.status")}</TableHead>
                <TableHead scope="col">{t("ingressPage.routes.columns.conditions")}</TableHead>
                <TableHead scope="col">{t("ingressPage.routes.columns.certificate")}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {rows.length === 0 ? (
                <TableRow className="hover:bg-transparent">
                  <TableCell colSpan={5} className="whitespace-normal">
                    {filter.trim() === "" ? (
                      <p className="text-sm text-muted-foreground">
                        {t("ingressPage.routes.noAttention")}
                      </p>
                    ) : (
                      <div className="flex flex-wrap items-center gap-3">
                        <p className="text-sm break-words">
                          {t("ingressPage.routes.noMatch", { filter: filter.trim() })}
                        </p>
                        <Button
                          type="button"
                          variant="outline"
                          size="sm"
                          className="max-md:h-11"
                          onClick={() => setFilter("")}
                        >
                          {t("ingressPage.routes.clearFilter")}
                        </Button>
                      </div>
                    )}
                  </TableCell>
                </TableRow>
              ) : null}
              {shown.map((r) => {
                if (r.kind !== "route") return <HeldRow key={heldKey(r)} row={r} envId={envId} />;
                const key = `${r.hostname}${r.path}:${r.service}`;
                const host = routeText(r);
                const expanded = open.has(key);
                const panel = `ingress-conditions-${key}`;
                const headline = routeHeadline(r.state || "unknown", r.stopped);
                const detail = detailOf(r, !swarm, t);
                return (
                  <Fragment key={key}>
                    <TableRow>
                      <TableCell className="align-top whitespace-normal">
                        <div className="flex items-start gap-1">
                          <Button
                            type="button"
                            variant="ghost"
                            size="icon-sm"
                            className="-ms-1 shrink-0 max-md:size-11 md:h-8"
                            aria-expanded={expanded}
                            aria-controls={expanded ? panel : undefined}
                            aria-label={t("ingressPage.routes.show", { host })}
                            onClick={() => toggle(key)}
                          >
                            <ChevronDown
                              aria-hidden="true"
                              className={cn(
                                "motion-safe:transition-transform",
                                expanded ? "rotate-180" : "-rotate-90",
                              )}
                            />
                          </Button>
                          <div className="min-w-0">
                            <span className="font-mono text-xs break-all">{host}</span>
                            <p className="text-xs text-muted-foreground">
                              {t(
                                r.strip
                                  ? "ingressPage.routes.targetStrip"
                                  : "ingressPage.routes.target",
                                { scheme: r.scheme, port: r.port },
                              )}
                            </p>
                          </div>
                        </div>
                      </TableCell>
                      <TableCell className="align-top whitespace-normal">
                        {r.stackId === "" ? (
                          <span className="text-sm break-words">{r.stackName}</span>
                        ) : (
                          <Link
                            to="/env/$envId/stacks/$stackId"
                            params={{ envId, stackId: r.stackId }}
                            className="rounded-sm text-sm break-words text-accent-ink hover:underline"
                          >
                            {r.stackName}
                          </Link>
                        )}
                        {swarm ? (
                          <Link
                            to="/env/$envId/services/$serviceId"
                            params={{ envId, serviceId: `${r.stackName}_${r.service}` }}
                            className="rounded-sm font-mono text-xs break-all text-accent-ink hover:underline"
                          >
                            {r.service}
                          </Link>
                        ) : (
                          <p className="font-mono text-xs break-all">{r.service}</p>
                        )}
                      </TableCell>
                      <TableCell className="align-top whitespace-normal">
                        <StatusDot state={headline.dot} label={t(headline.labelKey)} />
                        {detail ? (
                          <p className="text-xs break-words text-muted-foreground">{detail}</p>
                        ) : null}
                      </TableCell>
                      <TableCell className="align-top text-sm whitespace-normal">
                        {t("ingressPage.routes.ready", { ready: readyCount(r) })}
                      </TableCell>
                      <TableCell className="align-top text-sm whitespace-normal">
                        {certified.has(r.hostname.toLowerCase()) ? (
                          <Link
                            to="/env/$envId/ingress/certificates"
                            params={{ envId }}
                            search={{ host: r.hostname }}
                            aria-label={`${t("ingressPage.routes.viewCertificate")}: ${host}`}
                            className="rounded-sm text-accent-ink hover:underline max-md:inline-block max-md:py-3"
                          >
                            {t("ingressPage.routes.viewCertificate")}
                          </Link>
                        ) : (
                          t("ingressPage.routes.none")
                        )}
                      </TableCell>
                    </TableRow>
                    {expanded ? (
                      <TableRow className="hover:bg-transparent">
                        <TableCell
                          colSpan={5}
                          id={panel}
                          className="bg-muted/50 p-4 whitespace-normal"
                        >
                          <ol
                            aria-label={t("ingressPage.routes.conditionsList", { host })}
                            className="flex flex-col gap-3"
                          >
                            {(r.conditions ?? []).map((c) => {
                              const reason = reasonOf(c, r, t);
                              return (
                                <li key={c.code} className="flex flex-col gap-0.5">
                                  <div className="flex flex-wrap items-center gap-x-2 gap-y-0.5 text-sm">
                                    <StatusDot
                                      state={conditionDot(c)}
                                      label={t(`ingressPage.conditions.state.${c.state}`)}
                                    />
                                    <span>{t(`ingressPage.conditions.name.${c.code}`)}</span>
                                  </div>
                                  {reason ? (
                                    <p className="text-xs break-words text-muted-foreground">
                                      {reason}
                                    </p>
                                  ) : null}
                                </li>
                              );
                            })}
                          </ol>
                        </TableCell>
                      </TableRow>
                    ) : null}
                  </Fragment>
                );
              })}
            </TableBody>
          </Table>
          {rows.length > SHOWN ? (
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="self-start max-md:h-11"
              aria-expanded={all}
              onClick={() => setAll(!all)}
            >
              {all
                ? t("ingressPage.routes.showFewer")
                : t("ingressPage.routes.showAll", { count: rows.length })}
            </Button>
          ) : null}
        </>
      )}
    </div>
  );
}

function heldKey(r: IngressRoute): string {
  return `${r.kind}:${r.hostname}:${r.stackId}`;
}

// HeldRow is a row that is not a route (D-08, U-16): a name a stack holds that none of its services serves, or a name
// reserved under a domain that no stack holds. It has no disclosure and no conditions, only the reason it is not
// served, so a person who looks for a hostname finds it and learns why it does nothing.
function HeldRow({ row, envId }: { row: IngressRoute; envId: string }) {
  const t = useT();
  const claim = row.kind === "claim";
  const word = claim ? "claim" : "reserved";
  return (
    <TableRow>
      <TableCell className="align-top whitespace-normal">
        <div className="flex flex-wrap items-center gap-2">
          <span className="font-mono text-xs break-all">{row.hostname}</span>
          <Badge variant={claim ? "outline" : "secondary"} className="shrink-0">
            {t(`ingressPage.routes.${word}.badge`)}
          </Badge>
        </div>
      </TableCell>
      <TableCell className="align-top whitespace-normal">
        {row.stackName === "" ? (
          <span className="text-sm">{t("ingressPage.routes.none")}</span>
        ) : (
          <Link
            to="/env/$envId/stacks/$stackId"
            params={{ envId, stackId: row.stackId }}
            className="rounded-sm text-sm break-words text-accent-ink hover:underline"
          >
            {row.stackName}
          </Link>
        )}
      </TableCell>
      <TableCell className="align-top whitespace-normal">
        <StatusDot state="unknown" label={t(`ingressPage.routes.${word}.status`)} />
        <p className="text-xs break-words text-muted-foreground">
          {claim ? (
            t("ingressPage.routes.claim.detail")
          ) : (
            <Trans
              i18nKey="ingressPage.routes.reserved.detail"
              ns="ingressPage"
              components={{
                domains: (
                  <Link
                    to="/env/$envId/ingress/domains"
                    params={{ envId }}
                    className="rounded-sm text-accent-ink hover:underline"
                  />
                ),
              }}
            />
          )}
        </p>
      </TableCell>
      <TableCell className="align-top text-sm whitespace-normal">
        {t("ingressPage.routes.none")}
      </TableCell>
      <TableCell className="align-top text-sm whitespace-normal">
        {t("ingressPage.routes.none")}
      </TableCell>
    </TableRow>
  );
}
