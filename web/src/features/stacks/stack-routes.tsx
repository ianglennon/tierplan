// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useState } from "react";

import { type StackRoute, type StackRouteState, stackRoutesQuery } from "@/api/stack-routes";
import { type StackDetail, StacksLoadError } from "@/api/stacks";
import { StatusDot } from "@/components/status-dot";
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
import { SettingsCard } from "@/features/environments/settings-card";
import { routeHeadline } from "@/features/stacks/route-state";
import { useNeeds } from "@/hooks/use-needs";
import { keyExists, useT } from "@/i18n/use-t";

// At most this many rows show until the person asks for the rest (UI-SPEC S2, the CommitsCard pattern).
const SHOWN = 10;

interface StackRoutesProps {
  envId: string;
  stack: Pick<StackDetail, "id" | "stopped" | "target">;
}

// routeText is the hostname and the path joined, as a person types it; a route for the whole host is the hostname alone.
function routeText(route: StackRoute): string {
  return route.path === "" || route.path === "/"
    ? route.hostname
    : `${route.hostname}${route.path}`;
}

// StackRoutes is the Routes card on the stack's Overview (UI-SPEC S2, D-24, LBL-06): each route the stack holds, the
// service behind it and the state it is in, with the reason where the state has one. It draws nothing until the first
// answer, nothing for a stack that holds no route, and nothing for a caller the server refuses (403: the card hides
// itself, which is not access control). A failed read stays inside the card with a retry, and the rest of the
// Overview carries on. Every value renders as text: no hostname is a link (U-04).
export function StackRoutes({ envId, stack }: StackRoutesProps) {
  const t = useT();
  const deploy = useNeeds("stacks.deploy", envId);
  const [all, setAll] = useState(false);
  const query = useQuery(stackRoutesQuery(envId, stack.id));

  if (query.isPending) return <Skeleton className="h-24 w-full" aria-hidden="true" />;
  if (query.isError) {
    if (query.error instanceof StacksLoadError && query.error.status === 403) return null;
    return (
      <SettingsCard title={t("stackIngress.routes.title")}>
        <div className="flex flex-wrap items-center gap-3 text-sm">
          <span className="break-words">{t("stackIngress.routes.error")}</span>
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="max-md:h-11"
            disabled={query.isFetching}
            onClick={() => void query.refetch()}
          >
            {t("stackIngress.routes.retry")}
          </Button>
        </div>
      </SettingsCard>
    );
  }

  const routes = query.data.routes ?? [];
  if (routes.length === 0) return null;

  const shown = all ? routes : routes.slice(0, SHOWN);
  const notAttached = routes.some((r) => r.state === "not_attached");

  // detailOf is the second line of a route's State cell. A stopped stack says it once, above the table, so its
  // rows keep the dot and the label and do not repeat it.
  // A standalone host has one proxy and containers, not nodes and tasks, so two lines word it that way.
  const standalone = stack.target === "standalone";
  function detailOf(route: StackRoute): string {
    const state: StackRouteState = route.state;
    switch (state) {
      case "pending":
        return t(
          standalone
            ? "stackIngress.routes.state.pending.detailStandalone"
            : "stackIngress.routes.state.pending.detail",
        );
      case "not_attached":
      case "ingress_off":
        return t(`stackIngress.routes.state.${state}.detail`);
      case "waiting_certificate": {
        if (route.reason === "") return "";
        const key = `stackIngress.routes.reason.${route.reason}`;
        return keyExists(key) ? t(key) : route.reason;
      }
      case "backend_not_running":
        if (route.stopped)
          return stack.stopped ? "" : t("stackIngress.routes.state.backend_not_running.stopped");
        return t(
          standalone
            ? "stackIngress.routes.state.backend_not_running.noContainer"
            : "stackIngress.routes.state.backend_not_running.noTask",
          { service: route.service },
        );
      default:
        return "";
    }
  }

  return (
    <SettingsCard title={t("stackIngress.routes.title")}>
      {stack.stopped ? (
        <p className="text-sm text-muted-foreground">{t("stackIngress.routes.stopped")}</p>
      ) : null}
      <Table aria-label={t("stackIngress.routes.label")}>
        <TableHeader>
          <TableRow>
            <TableHead scope="col">{t("stackIngress.routes.columns.route")}</TableHead>
            <TableHead scope="col">{t("stackIngress.routes.columns.service")}</TableHead>
            <TableHead scope="col">{t("stackIngress.routes.columns.state")}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {shown.map((r) => {
            const detail = detailOf(r);
            const headline = routeHeadline(r.state, r.stopped);
            return (
              <TableRow key={`${r.hostname}${r.path}:${r.service}`}>
                <TableCell className="align-top font-mono text-xs break-all whitespace-normal">
                  {routeText(r)}
                </TableCell>
                <TableCell className="align-top font-mono text-xs break-all whitespace-normal">
                  {r.service}
                </TableCell>
                <TableCell className="align-top whitespace-normal">
                  <StatusDot state={headline.dot} label={t(headline.labelKey)} />
                  {detail ? (
                    <p className="text-xs break-words text-muted-foreground">{detail}</p>
                  ) : null}
                </TableCell>
              </TableRow>
            );
          })}
        </TableBody>
      </Table>
      {routes.length > SHOWN ? (
        <Button
          type="button"
          variant="outline"
          size="sm"
          className="self-start max-md:h-11"
          aria-expanded={all}
          onClick={() => setAll(!all)}
        >
          {all
            ? t("stackIngress.routes.showFewer")
            : t("stackIngress.routes.showAll", { count: routes.length })}
        </Button>
      ) : null}
      {notAttached ? (
        <div className="flex flex-wrap items-center gap-3">
          <p className="text-xs text-muted-foreground">{t("stackIngress.routes.notAttached")}</p>
          {deploy.allowed ? (
            <Button asChild variant="outline" size="sm" className="max-md:h-11">
              <Link to="/env/$envId/stacks/$stackId/review" params={{ envId, stackId: stack.id }}>
                {t("stackIngress.routes.reviewAndDeploy")}
              </Link>
            </Button>
          ) : null}
        </div>
      ) : null}
    </SettingsCard>
  );
}
