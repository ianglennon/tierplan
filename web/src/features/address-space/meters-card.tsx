// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { Link } from "@tanstack/react-router";
import { Info } from "lucide-react";

import type { AddressPool, AddressSpace, OverlayUsage } from "@/api/address-space";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { UsageMeter } from "@/features/address-space/usage-meter";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

// Past this many pools the list scrolls inside the card (UI-SPEC "Shared state and overflow rules").
const POOLS_BEFORE_SCROLL = 8;

type PoolSource = AddressPool["source"];

// The estimated note and tooltip are worded by where the pools came from (UI-SPEC amendment,
// user decision 1). A standalone host with no pool at all counts against Docker's defaults.
export function estimateSource(data: AddressSpace): "daemon" | "defaults" | "entered" {
  const source: PoolSource | undefined = data.pools?.[0]?.source;
  return source === "daemon" || source === "entered" ? source : "defaults";
}

function Title({ children }: { children: string }) {
  return <h3 className="text-base font-semibold break-all">{children}</h3>;
}

// PoolRow is one address pool: its range in mono, the meter and what the numbers mean. On a
// standalone host the numbers are estimates, so a pool at 90% says it never changes health.
export function PoolRow({
  pool,
  measured = true,
}: {
  pool: AddressPool;
  // False when the swarm could not be measured: the row says so instead of showing old numbers.
  measured?: boolean;
}) {
  const t = useT();
  const state = measured ? pool.state : "unknown";
  const caption =
    state === "unknown"
      ? t("addressSpace.meters.pool.captionUnknown", { mask: pool.mask })
      : pool.largestFreeSubnets > 0
        ? t("addressSpace.meters.pool.captionFree", {
            used: pool.used,
            capacity: pool.capacity,
            mask: pool.mask,
            cidr: pool.largestFree,
            count: pool.largestFreeSubnets,
          })
        : t("addressSpace.meters.pool.captionFull", {
            used: pool.used,
            capacity: pool.capacity,
            mask: pool.mask,
          });
  return (
    <section aria-label={pool.cidr} className="flex flex-col gap-1.5">
      <h3 className="font-mono text-base font-semibold break-all">{pool.cidr}</h3>
      <UsageMeter
        used={pool.used}
        capacity={pool.capacity}
        label={pool.cidr}
        unit="subnets"
        estimated={pool.estimated}
        state={state}
        percent={pool.percent}
      />
      <p className="text-xs break-all text-muted-foreground">{caption}</p>
      {pool.estimated && state === "almost_full" ? (
        <p className="text-xs text-muted-foreground">{t("addressSpace.meters.estimatedHealth")}</p>
      ) : null}
    </section>
  );
}

// IngressRow is the swarm's routing-mesh network with its own meter. Outside every pool it is
// counted in no pool row, so it carries the "Outside the pool" badge; inside a pool it is counted
// there and has no badge.
export function IngressRow({ ingress, noManager }: { ingress: OverlayUsage; noManager: boolean }) {
  const t = useT();
  const title = t("addressSpace.meters.ingress.title");
  const unmeasured = noManager || !ingress.measured;
  return (
    <section aria-label={title} className="flex flex-col gap-1.5">
      <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
        <Title>{title}</Title>
        {(ingress.subnets ?? []).length > 0 ? (
          <span className="font-mono text-xs break-all text-muted-foreground">
            {(ingress.subnets ?? []).join(", ")}
          </span>
        ) : null}
        {ingress.outsidePool ? (
          <Tooltip>
            <TooltipTrigger asChild>
              <Badge variant="outline" tabIndex={0}>
                {t("addressSpace.meters.ingress.outside")}
              </Badge>
            </TooltipTrigger>
            <TooltipContent className="max-w-80">
              {t("addressSpace.meters.ingress.outsideTip")}
            </TooltipContent>
          </Tooltip>
        ) : null}
      </div>
      <UsageMeter
        used={ingress.used}
        capacity={ingress.capacity}
        label={title}
        unit="addresses"
        state={unmeasured ? "unknown" : ingress.state}
        percent={ingress.percent}
        notMeasuredHint={noManager ? undefined : t("addressSpace.meters.notMeasuredTip")}
      />
      {unmeasured ? null : (
        <p className="text-xs text-muted-foreground">
          {t("addressSpace.meters.ingress.caption", {
            used: ingress.used,
            capacity: ingress.capacity,
          })}
        </p>
      )}
    </section>
  );
}

// fillingOverlays are the overlays at 70% or more, the fullest first. The ingress network has a
// row of its own, so it is not listed twice.
export function fillingOverlays(overlays: readonly OverlayUsage[]): OverlayUsage[] {
  return overlays
    .filter(
      (o) => !o.ingress && o.measured && (o.state === "getting_full" || o.state === "almost_full"),
    )
    .sort(
      (a, b) =>
        b.used * a.capacity - a.used * b.capacity ||
        (a.name < b.name ? -1 : a.name > b.name ? 1 : 0),
    );
}

// FillingRows is the "Networks filling up" group. Each network name is the one link of its row.
export function FillingRows({
  envId,
  overlays,
}: {
  envId: string;
  overlays: readonly OverlayUsage[];
}) {
  const t = useT();
  const filling = fillingOverlays(overlays);
  if (filling.length === 0) return null;
  return (
    <div className="flex flex-col gap-4">
      <h3 className="text-base font-semibold">{t("addressSpace.meters.filling.title")}</h3>
      {filling.map((o) => (
        <section key={o.id} aria-label={o.name} className="flex flex-col gap-1.5">
          <div className="flex flex-wrap items-baseline gap-x-2 gap-y-1">
            <Link
              to="/env/$envId/networks/$networkId"
              params={{ envId, networkId: o.id }}
              className="rounded-sm font-mono text-sm break-all text-accent-ink hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
            >
              {o.name}
            </Link>
            {(o.subnets ?? []).length > 0 ? (
              <span className="font-mono text-xs break-all text-muted-foreground">
                {(o.subnets ?? []).join(", ")}
              </span>
            ) : null}
          </div>
          <UsageMeter
            used={o.used}
            capacity={o.capacity}
            label={o.name}
            unit="addresses"
            state={o.state}
            percent={o.percent}
          />
          <p className="text-xs text-muted-foreground">
            {t("addressSpace.meters.filling.caption", { used: o.used, capacity: o.capacity })}
          </p>
        </section>
      ))}
    </div>
  );
}

// MetersCardLoading is the card with its title at once and three skeleton rows at the height of
// the real ones (title 20px, bar 8px, caption 16px).
export function MetersCardLoading() {
  const t = useT();
  return (
    <Card aria-busy="true">
      <CardHeader>
        <CardTitle>{t("addressSpace.meters.title")}</CardTitle>
      </CardHeader>
      <CardContent className="flex flex-col gap-4">
        <span className="sr-only">{t("addressSpace.meters.loading")}</span>
        {[0, 1, 2].map((i) => (
          <div key={i} data-testid="meter-skeleton" className="flex flex-col gap-1.5">
            <Skeleton className="h-5 w-40" />
            <Skeleton className="h-2 w-full" />
            <Skeleton className="h-4 w-2/3" />
          </div>
        ))}
      </CardContent>
    </Card>
  );
}

interface MetersCardProps {
  envId: string;
  data: AddressSpace;
}

// MetersCard is the Pools card: one row per pool in the order the swarm lists them, the ingress
// row and the networks that are filling up. A standalone host's numbers are all estimates and say
// so; a swarm's are measured from the swarm.
export function MetersCard({ envId, data }: MetersCardProps) {
  const t = useT();
  const pools = data.pools ?? [];
  const standalone = data.envType === "standalone";
  const noManager = data.notMeasured === "no_manager";
  const source = estimateSource(data);
  return (
    <Card>
      <CardHeader>
        <div className="flex flex-wrap items-center gap-2">
          <CardTitle>{t("addressSpace.meters.title")}</CardTitle>
          {standalone ? (
            <Tooltip>
              <TooltipTrigger asChild>
                <Badge variant="outline" tabIndex={0}>
                  <Info aria-hidden="true" />
                  {t("addressSpace.meters.estimatedBadge")}
                </Badge>
              </TooltipTrigger>
              <TooltipContent className="max-w-80">
                {t(`addressSpace.notes.estimatedTip.${source}`)}
              </TooltipContent>
            </Tooltip>
          ) : null}
        </div>
        {!standalone && !noManager ? (
          <p className="text-xs text-muted-foreground">{t("addressSpace.meters.swarmSource")}</p>
        ) : null}
      </CardHeader>
      <CardContent className="flex flex-col gap-4">
        <div
          className={cn(
            "flex flex-col gap-4",
            pools.length > POOLS_BEFORE_SCROLL && "max-h-96 overflow-y-auto",
          )}
        >
          {pools.map((pool) => (
            <PoolRow key={pool.cidr} pool={pool} measured={!noManager} />
          ))}
        </div>
        {pools.length === 0 ? (
          <p className="text-sm text-muted-foreground">
            {noManager ? t("addressSpace.states.notMeasured") : t("addressSpace.meters.none")}
          </p>
        ) : null}
        {data.ingress ? <IngressRow ingress={data.ingress} noManager={noManager} /> : null}
        {noManager ? null : <FillingRows envId={envId} overlays={data.overlays ?? []} />}
      </CardContent>
    </Card>
  );
}
