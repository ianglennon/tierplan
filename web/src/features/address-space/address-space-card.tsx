// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { Link } from "@tanstack/react-router";
import { ChevronRight, Info } from "lucide-react";

import type { EnvironmentItem } from "@/api/environments";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { UsageMeter } from "@/features/address-space/usage-meter";
import { usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

type Summary = EnvironmentItem["addressSpace"];

interface AddressSpaceCardProps {
  envId: string;
  // The environment's fullest pool or network, from the environments list. Null until the
  // address space has been measured, and while the environment is offline.
  summary?: Summary;
  loading?: boolean;
  error?: boolean;
}

// The estimate's muted line shows from this percent: an estimate never changes the health, so
// the card says so where an operator would expect a warning (UI-SPEC "Dashboard card").
const ESTIMATE_NOTE_FROM = 90;

function OpenLink({ envId }: { envId: string }) {
  const t = useT();
  return (
    <Button asChild variant="ghost" size="sm" className="-ms-2.5 self-start">
      <Link to="/env/$envId/address-space" params={{ envId }}>
        {t("dashboard.addressCard.open")}
        <ChevronRight aria-hidden="true" data-icon="inline-end" />
      </Link>
    </Button>
  );
}

// AddressSpaceCard is the "Address space" card on the environment dashboard (D-06 item 1): the
// fullest pool or network with its meter, how many networks are orphaned, and the way to the
// page. It is hidden from someone who may not read networks (D-07), the server filtering the
// same numbers out of the environments list by environments.read.
export function AddressSpaceCard({ envId, summary, loading, error }: AddressSpaceCardProps) {
  const t = useT();
  const canRead = usePermission("networks.read", envId);
  if (!canRead) return null;
  const title = t("dashboard.addressCard.title");

  if (loading) {
    return (
      <section aria-label={title} aria-busy="true">
        <Card>
          <CardHeader>
            <CardTitle>{title}</CardTitle>
          </CardHeader>
          <CardContent className="flex flex-col gap-1.5">
            <span className="sr-only">{t("dashboard.addressCard.loading")}</span>
            <Skeleton className="h-5 w-40" aria-hidden="true" />
            <Skeleton className="h-2 w-full" aria-hidden="true" />
            <Skeleton className="h-4 w-2/3" aria-hidden="true" />
            <Skeleton className="h-7 w-36" aria-hidden="true" />
          </CardContent>
        </Card>
      </section>
    );
  }

  if (error) {
    return (
      <section aria-label={title}>
        <Card>
          <CardHeader>
            <CardTitle>{title}</CardTitle>
          </CardHeader>
          <CardContent className="flex flex-col gap-2">
            <p className="text-sm">{t("dashboard.addressCard.error")}</p>
            <OpenLink envId={envId} />
          </CardContent>
        </Card>
      </section>
    );
  }

  const measured = summary && summary.state !== "unknown" ? summary : null;
  return (
    <section aria-label={title}>
      <Card>
        <CardHeader>
          <div className="flex flex-wrap items-center gap-2">
            <CardTitle>{title}</CardTitle>
            {measured?.estimated ? (
              <Badge variant="outline">
                <Info aria-hidden="true" />
                {t("dashboard.addressCard.estimatedBadge")}
              </Badge>
            ) : null}
          </div>
        </CardHeader>
        <CardContent className="flex flex-col gap-3">
          {measured ? (
            <>
              <div className="flex flex-col gap-1.5">
                <h3
                  className={
                    measured.worstKind === "pool"
                      ? "font-mono text-base font-semibold break-all"
                      : "text-base font-semibold break-all"
                  }
                >
                  {measured.worstName}
                </h3>
                <UsageMeter
                  used={measured.used}
                  capacity={measured.capacity}
                  label={measured.worstName}
                  unit={measured.unit}
                  estimated={measured.estimated}
                  state={measured.state}
                  percent={measured.percent}
                />
                <p className="text-xs text-muted-foreground">
                  {t("dashboard.addressCard.caption", {
                    used: measured.used,
                    capacity: measured.capacity,
                    unit: t(`addressSpace.meter.unit.${measured.unit}`),
                  })}
                </p>
                {measured.estimated && measured.percent >= ESTIMATE_NOTE_FROM ? (
                  <p className="text-xs text-muted-foreground">
                    {t("dashboard.addressCard.estimatedHealth")}
                  </p>
                ) : null}
              </div>
              {measured.orphaned > 0 ? (
                <p className="text-xs text-muted-foreground">
                  <Link
                    to="/env/$envId/address-space"
                    params={{ envId }}
                    hash="orphans"
                    className="rounded-sm underline underline-offset-3 hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
                  >
                    {t("dashboard.addressCard.orphaned", { count: measured.orphaned })}
                  </Link>
                </p>
              ) : null}
            </>
          ) : (
            <div className="flex flex-col gap-1">
              <p className="text-sm font-semibold">{t("dashboard.addressCard.notMeasured")}</p>
              <p className="text-xs text-muted-foreground">
                {t("dashboard.addressCard.notMeasuredBody")}
              </p>
            </div>
          )}
          <OpenLink envId={envId} />
        </CardContent>
      </Card>
    </section>
  );
}
