// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { BookOpen, Info, TriangleAlert } from "lucide-react";

import { type AddressSpace, addressSpaceQuery } from "@/api/address-space";
import { NoPermission } from "@/components/full-page-state";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Spinner } from "@/components/ui/spinner";
import { KeptList } from "@/features/address-space/kept-list";
import {
  estimateSource,
  MetersCard,
  MetersCardLoading,
} from "@/features/address-space/meters-card";
import { OrphansCard } from "@/features/address-space/orphans-card";
import { type MapSearch, SubnetMap } from "@/features/address-space/subnet-map";
import type { MeterState } from "@/features/address-space/usage-meter";
import { NodesBanner } from "@/features/environments/nodes-banner";
import { useEnvName } from "@/features/resources/use-env-name";
import { usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

interface Worst {
  name: string;
  percent: number;
  state: "getting_full" | "almost_full";
}

// worstMeter is the fullest pool, ingress or overlay that is at 70% or more, or none. The state
// the server decided on the exact ratio is the one used; an unmeasured meter is never the worst.
export function worstMeter(data: AddressSpace): Worst | undefined {
  const candidates: {
    name: string;
    used: number;
    capacity: number;
    percent: number;
    state: MeterState;
  }[] = [
    ...(data.pools ?? []).map((p) => ({ ...p, name: p.cidr })),
    ...(data.overlays ?? []).filter((o) => o.measured),
    ...(data.ingress?.measured ? [data.ingress] : []),
  ];
  let worst: (typeof candidates)[number] | undefined;
  for (const c of candidates) {
    if (c.state !== "getting_full" && c.state !== "almost_full") continue;
    if (!worst || c.used * worst.capacity > worst.used * c.capacity) worst = c;
  }
  if (!worst || (worst.state !== "getting_full" && worst.state !== "almost_full")) return undefined;
  return { name: worst.name, percent: worst.percent, state: worst.state };
}

// AddressSpacePage is where an operator sees how much of the environment's network address space
// is used (NET-02, NET-03): the pools, the ingress network and the overlays that are filling up.
// Nothing on it is labelled live or cached: Docker is the source of truth and the page reads it
// again on an event and every 30 s.
export function AddressSpacePage({
  envId,
  search,
  onSearchChange,
}: {
  envId: string;
  // The map's search, filter and sort, from the URL.
  search: MapSearch;
  onSearchChange: (patch: Partial<MapSearch>) => void;
}) {
  const t = useT();
  const allowed = usePermission("networks.read", envId);
  const query = useQuery({ ...addressSpaceQuery(envId), enabled: allowed });
  const envName = useEnvName(envId);
  if (!allowed) return <NoPermission />;
  const data = query.data;
  const failed = query.isError && !data;
  const worst = data ? worstMeter(data) : undefined;
  return (
    <div className="mx-auto flex w-full max-w-7xl flex-col gap-4">
      <header className="flex flex-wrap items-start justify-between gap-x-4 gap-y-2">
        <div className="flex min-w-0 flex-col gap-1">
          <h1 className="text-2xl font-semibold">{t("addressSpace.page.title")}</h1>
          <p className="text-sm text-muted-foreground">{t("addressSpace.page.caption")}</p>
        </div>
        <Button asChild variant="outline" className="max-md:h-11">
          <Link to="/env/$envId/address-space/guide" params={{ envId }}>
            <BookOpen aria-hidden="true" />
            {t("poolGuide.header.action")}
          </Link>
        </Button>
      </header>

      {worst ? (
        // role="status" is announced politely, so a refetch that crosses 70% or 90% is heard once
        // and focus never moves.
        <Alert variant="warning" role="status">
          <TriangleAlert aria-hidden="true" />
          <AlertTitle>
            {t(
              worst.state === "almost_full"
                ? "addressSpace.page.summary.almostFull"
                : "addressSpace.page.summary.gettingFull",
            )}
          </AlertTitle>
          <AlertDescription className="text-foreground break-words">
            {t("addressSpace.page.summary.body", { name: worst.name, percent: worst.percent })}
          </AlertDescription>
          <div className="col-start-2 mt-2">
            <Button asChild variant="outline" size="sm" className="max-md:h-11">
              <Link to="/env/$envId/address-space/guide" params={{ envId }}>
                {t("poolGuide.header.open")}
              </Link>
            </Button>
          </div>
        </Alert>
      ) : null}

      {data?.envType === "standalone" ? (
        <Alert role="note">
          <Info aria-hidden="true" />
          <AlertDescription>
            {t(`addressSpace.notes.estimated.${estimateSource(data)}`)}
          </AlertDescription>
        </Alert>
      ) : null}

      {data?.notMeasured === "no_manager" ? (
        <Alert variant="warning" role="status">
          <TriangleAlert aria-hidden="true" />
          <AlertTitle>{t("addressSpace.page.noManager.title")}</AlertTitle>
          <AlertDescription className="text-foreground">
            {t("addressSpace.page.noManager.body")}
          </AlertDescription>
        </Alert>
      ) : null}

      {data && data.envType === "swarm" ? <NodesBanner notReporting={data.notReporting} /> : null}

      {failed ? (
        <Alert variant="destructive">
          <AlertTitle>{t("addressSpace.page.error.title")}</AlertTitle>
          <AlertDescription className="flex flex-wrap items-center gap-3">
            <span>{t("addressSpace.page.error.body")}</span>
            <Button
              variant="outline"
              size="sm"
              disabled={query.isFetching}
              onClick={() => void query.refetch()}
            >
              {query.isFetching ? <Spinner aria-hidden="true" /> : null}
              {t("addressSpace.page.error.retry")}
            </Button>
          </AlertDescription>
        </Alert>
      ) : data ? (
        <>
          <MetersCard envId={envId} data={data} />
          <SubnetMap
            envId={envId}
            rows={data.networks ?? []}
            total={data.networksTotal}
            truncated={data.networksTruncated}
            swarm={data.envType === "swarm"}
            orphans={data.orphans ?? []}
            kept={data.kept ?? []}
            search={search}
            onSearchChange={onSearchChange}
          />
          <OrphansCard
            envId={envId}
            envName={envName}
            orphans={data.orphans ?? []}
            withheld={data.orphansWithheld}
            swarm={data.envType === "swarm"}
            graceHours={data.graceHours}
          />
          <KeptList
            envId={envId}
            envName={envName}
            kept={data.kept ?? []}
            swarm={data.envType === "swarm"}
          />
        </>
      ) : (
        <>
          <MetersCardLoading />
          <SubnetMap
            envId={envId}
            rows={undefined}
            total={0}
            truncated={false}
            swarm={false}
            search={search}
            onSearchChange={onSearchChange}
          />
          <OrphansCard
            envId={envId}
            envName={envName}
            orphans={undefined}
            withheld={undefined}
            swarm={false}
          />
        </>
      )}
    </div>
  );
}
