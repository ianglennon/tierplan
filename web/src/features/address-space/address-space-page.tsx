// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useQuery } from "@tanstack/react-query";

import { addressSpaceQuery } from "@/api/address-space";
import { NoPermission } from "@/components/full-page-state";
import { MetersCard } from "@/features/address-space/meters-card";
import { usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

// AddressSpacePage is where an operator sees how much of the environment's network address space
// is used (NET-02, NET-03): the pools, the ingress network and the overlays that are filling up.
export function AddressSpacePage({ envId }: { envId: string }) {
  const t = useT();
  const allowed = usePermission("networks.read", envId);
  const query = useQuery({ ...addressSpaceQuery(envId), enabled: allowed });
  if (!allowed) return <NoPermission />;
  return (
    <div className="mx-auto flex w-full max-w-7xl flex-col gap-4">
      <header className="flex flex-col gap-1">
        <h1 className="text-2xl font-semibold">{t("addressSpace.page.title")}</h1>
        <p className="text-sm text-muted-foreground">{t("addressSpace.page.caption")}</p>
      </header>
      {query.data ? <MetersCard pools={query.data.pools ?? []} /> : null}
    </div>
  );
}
