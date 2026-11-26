// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";

import { type IngressRoute, ingressRoutesQuery } from "@/api/ingress";
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
import { useT } from "@/i18n/use-t";

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

// RoutesTab is the Routes tab of the Ingress page (UIX-02, UI-SPEC S2): each route of the environment with the
// stack and service behind it, its headline state drawn through the map the stack page uses, and how many of its
// five conditions hold. A held name, a reserved name and a TierPlan route carry no state and are not drawn here.
// Every value renders as text: no hostname is a link (U-08).
export function RoutesTab({ envId }: { envId: string }) {
  const t = useT();
  const query = useQuery(ingressRoutesQuery(envId));

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
          const headline = routeHeadline(r.state || "unknown", r.stopped);
          return (
            <TableRow key={`${r.hostname}${r.path}:${r.service}`}>
              <TableCell className="align-top whitespace-normal">
                <span className="font-mono text-xs break-all">{routeText(r)}</span>
                <p className="text-xs text-muted-foreground">
                  {t(r.strip ? "ingressPage.routes.targetStrip" : "ingressPage.routes.target", {
                    scheme: r.scheme,
                    port: r.port,
                  })}
                </p>
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
          );
        })}
      </TableBody>
    </Table>
  );
}
