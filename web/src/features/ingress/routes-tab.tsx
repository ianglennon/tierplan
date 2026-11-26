// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { ChevronDown } from "lucide-react";
import { Fragment, useState } from "react";

import type { HealthState } from "@/api/environments";

import { type IngressRoute, ingressRoutesQuery, type RouteCondition } from "@/api/ingress";
import { StatusDot } from "@/components/status-dot";
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
import { routeHeadline } from "@/features/stacks/route-state";
import { keyExists, useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

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

// RoutesTab is the Routes tab of the Ingress page (UIX-02, UI-SPEC S2): each route of the environment with the
// stack and service behind it, its headline state drawn through the map the stack page uses, and how many of its
// five conditions hold. A held name, a reserved name and a TierPlan route carry no state and are not drawn here.
// Every value renders as text: no hostname is a link (U-08).
export function RoutesTab({ envId }: { envId: string }) {
  const t = useT();
  const query = useQuery(ingressRoutesQuery(envId));
  // The routes whose conditions are open, by hostname, path and service. It is local to the tab, so it survives a
  // refetch, and nothing opens by itself.
  const [open, setOpen] = useState<ReadonlySet<string>>(new Set());
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

  const rows = (query.data.routes ?? []).filter((r) => r.kind === "route" && r.state !== "");

  return (
    <Table aria-label={t("ingressPage.routes.label")}>
      <TableHeader>
        <TableRow>
          <TableHead scope="col">{t("ingressPage.routes.columns.route")}</TableHead>
          <TableHead scope="col">{t("ingressPage.routes.columns.stack")}</TableHead>
          <TableHead scope="col">{t("ingressPage.routes.columns.status")}</TableHead>
          <TableHead scope="col">{t("ingressPage.routes.columns.conditions")}</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {rows.map((r) => {
          const key = `${r.hostname}${r.path}:${r.service}`;
          const host = routeText(r);
          const expanded = open.has(key);
          const panel = `ingress-conditions-${key}`;
          const headline = routeHeadline(r.state || "unknown", r.stopped);
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
                          r.strip ? "ingressPage.routes.targetStrip" : "ingressPage.routes.target",
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
                  <p className="font-mono text-xs break-all">{r.service}</p>
                </TableCell>
                <TableCell className="align-top whitespace-normal">
                  <StatusDot state={headline.dot} label={t(headline.labelKey)} />
                </TableCell>
                <TableCell className="align-top text-sm whitespace-normal">
                  {t("ingressPage.routes.ready", { ready: readyCount(r) })}
                </TableCell>
              </TableRow>
              {expanded ? (
                <TableRow className="hover:bg-transparent">
                  <TableCell colSpan={4} id={panel} className="bg-muted/50 p-4 whitespace-normal">
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
                              <p className="text-xs break-words text-muted-foreground">{reason}</p>
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
  );
}
