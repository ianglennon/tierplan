// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import type { AddressPool } from "@/api/address-space";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { UsageMeter } from "@/features/address-space/usage-meter";
import { useT } from "@/i18n/use-t";

// PoolRow is one address pool: its range in mono, the meter and what the numbers mean.
export function PoolRow({ pool }: { pool: AddressPool }) {
  const t = useT();
  const caption =
    pool.largestFreeSubnets > 0
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
        state={pool.state}
        percent={pool.percent}
      />
      <p className="text-xs text-muted-foreground">{caption}</p>
    </section>
  );
}

interface MetersCardProps {
  pools: readonly AddressPool[];
}

// MetersCard is the Pools card: one row per pool in the order the swarm lists them.
export function MetersCard({ pools }: MetersCardProps) {
  const t = useT();
  return (
    <Card>
      <CardHeader>
        <CardTitle>{t("addressSpace.meters.title")}</CardTitle>
        <p className="text-xs text-muted-foreground">{t("addressSpace.meters.swarmSource")}</p>
      </CardHeader>
      <CardContent className="flex flex-col gap-4">
        {pools.map((pool) => (
          <PoolRow key={pool.cidr} pool={pool} />
        ))}
      </CardContent>
    </Card>
  );
}
