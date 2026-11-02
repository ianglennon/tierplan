// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { CircleCheck, Lightbulb } from "lucide-react";
import type { ReactNode } from "react";

import { addressSettingsQuery, addressSpaceQuery } from "@/api/address-space";
import {
  Empty,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@/components/ui/empty";
import { Item, ItemContent, ItemMedia } from "@/components/ui/item";
import { Skeleton } from "@/components/ui/skeleton";
import { parseCidr } from "@/features/networks/cidr";
import { useT } from "@/i18n/use-t";

// The size a small subnet has when the person has not chosen one: what the settings card offers
// first.
const DEFAULT_SMALL_MASK = 26;

// addressesHeld is how many IPv4 addresses the subnets hold between them. A subnet that is not
// IPv4 is not counted: the pools are IPv4.
export function addressesHeld(subnets: readonly string[] | null | undefined): number {
  let total = 0;
  for (const s of subnets ?? []) {
    const cidr = parseCidr(s);
    if (cidr !== null && cidr.family === 4) total += 2 ** (32 - cidr.bits);
  }
  return total;
}

function Tip({ title, children }: { title: string; children: ReactNode }) {
  return (
    <li>
      <Item variant="outline" className="items-start">
        <ItemMedia variant="icon" aria-hidden="true">
          <Lightbulb />
        </ItemMedia>
        <ItemContent className="min-w-0 gap-2">
          <h3 className="text-sm font-medium">{title}</h3>
          {children}
        </ItemContent>
      </Item>
    </li>
  );
}

const LINK = "w-fit rounded-sm text-sm text-accent-ink underline underline-offset-4 max-md:py-3";

// FreeSpaceTips is the Free up space tab (D-23 part 3): three things to try before anything is
// rebuilt, with the numbers read from this environment's address space and settings. They give
// space back without downtime.
export function FreeSpaceTips({ envId }: { envId: string }) {
  const t = useT();
  const space = useQuery(addressSpaceQuery(envId));
  const settings = useQuery(addressSettingsQuery(envId));
  if (space.isPending) {
    return (
      <Skeleton
        aria-busy="true"
        aria-label={t("poolGuide.freeSpace.loading")}
        className="h-40 w-full"
      />
    );
  }
  if (space.isError) {
    return (
      <p role="alert" className="text-sm text-destructive">
        {t("poolGuide.freeSpace.loadFailed")}
      </p>
    );
  }
  const data = space.data;
  const orphans = (data.orphans ?? []).filter(
    (o) => o.class === "orphaned" || o.class === "unused",
  );
  const orphaned = orphans.filter((o) => o.class === "orphaned").length;
  const unused = orphans.length - orphaned;
  const addresses = orphans.reduce((sum, o) => sum + addressesHeld(o.subnets), 0);
  const filling = (data.overlays ?? []).filter(
    (o) => o.measured && (o.state === "getting_full" || o.state === "almost_full"),
  );
  const small = settings.data?.smallSubnets;
  const mask = small?.enabled ? small.mask : DEFAULT_SMALL_MASK;

  const subnets = (
    <Tip title={t("poolGuide.freeSpace.subnets.title")}>
      <p className="text-sm text-muted-foreground">
        {t("poolGuide.freeSpace.subnets.body", { mask, times: 2 ** (mask - 24) })}
      </p>
      {small?.enabled ? (
        <p className="flex items-center gap-1.5 text-sm font-medium">
          <CircleCheck aria-hidden="true" className="size-4 shrink-0" />
          {t("poolGuide.freeSpace.subnets.on", { mask: small.mask })}
        </p>
      ) : null}
      <Link
        to="/settings/environments/$envId"
        params={{ envId }}
        hash="default-subnets"
        className={LINK}
      >
        {t("poolGuide.freeSpace.subnets.open")}
      </Link>
    </Tip>
  );

  return (
    <div className="flex flex-col gap-4">
      <p className="text-sm text-muted-foreground">{t("poolGuide.freeSpace.intro")}</p>
      <ul className="flex flex-col gap-4">
        {orphans.length === 0 && filling.length === 0 ? (
          <li>
            <Empty className="border">
              <EmptyHeader>
                <EmptyMedia variant="icon">
                  <Lightbulb aria-hidden="true" />
                </EmptyMedia>
                <EmptyTitle>{t("poolGuide.freeSpace.emptyTitle")}</EmptyTitle>
                <EmptyDescription>{t("poolGuide.freeSpace.emptyBody")}</EmptyDescription>
              </EmptyHeader>
            </Empty>
          </li>
        ) : (
          <>
            <Tip title={t("poolGuide.freeSpace.orphans.title")}>
              <p className="text-sm text-muted-foreground">
                {orphans.length > 0
                  ? t("poolGuide.freeSpace.orphans.body", {
                      orphaned: orphaned.toLocaleString("en"),
                      unused: unused.toLocaleString("en"),
                      addresses: addresses.toLocaleString("en"),
                    })
                  : t("poolGuide.freeSpace.orphans.none")}
              </p>
              {orphans.length > 0 ? (
                <Link
                  to="/env/$envId/address-space"
                  params={{ envId }}
                  hash="orphans"
                  className={LINK}
                >
                  {t("poolGuide.freeSpace.orphans.open")}
                </Link>
              ) : null}
            </Tip>
            {subnets}
            <Tip title={t("poolGuide.freeSpace.crowded.title")}>
              <p className="text-sm text-muted-foreground">
                {t("poolGuide.freeSpace.crowded.body")}
              </p>
              {filling.length > 0 ? (
                <ul className="flex flex-col gap-1 text-sm">
                  {filling.map((o) => (
                    <li key={o.id}>
                      <Link
                        to="/env/$envId/networks/$networkId"
                        params={{ envId, networkId: o.id }}
                        className="rounded-sm text-accent-ink underline underline-offset-4 max-md:inline-block max-md:py-3"
                      >
                        {t("poolGuide.freeSpace.crowded.item", {
                          name: o.name,
                          percent: o.percent,
                        })}
                      </Link>
                    </li>
                  ))}
                </ul>
              ) : (
                <p className="text-sm">{t("poolGuide.freeSpace.crowded.none")}</p>
              )}
            </Tip>
          </>
        )}
        {orphans.length === 0 && filling.length === 0 ? subnets : null}
      </ul>
    </div>
  );
}
