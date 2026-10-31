// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { Info } from "lucide-react";
import { useState } from "react";

import { environmentsQuery } from "@/api/environments";
import { NoPermission } from "@/components/full-page-state";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Skeleton } from "@/components/ui/skeleton";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { DEFAULT_SWARM_INPUTS, type SwarmInputs } from "@/features/pool-guide/generate";
import { NewSwarmForm } from "@/features/pool-guide/new-swarm-form";
import { usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

// The four parts of the guidance (D-23), as the `part` search value names them.
export const GUIDE_PARTS = ["free-space", "new-swarm", "rebuild", "standalone"] as const;
export type GuidePart = (typeof GUIDE_PARTS)[number];

const TAB_KEYS: Record<GuidePart, string> = {
  "free-space": "poolGuide.tabs.freeSpace",
  "new-swarm": "poolGuide.tabs.newSwarm",
  rebuild: "poolGuide.tabs.rebuild",
  standalone: "poolGuide.tabs.standalone",
};

function Breadcrumb({ envId }: { envId: string }) {
  const t = useT();
  return (
    <nav aria-label={t("poolGuide.breadcrumb.label")}>
      <ol className="flex flex-wrap items-center gap-x-2 text-xs text-muted-foreground [&>li:not(:last-child)]:after:ms-2 [&>li:not(:last-child)]:after:content-['/']">
        <li>
          <Link
            to="/env/$envId/address-space"
            params={{ envId }}
            className="rounded-sm text-accent-ink hover:underline"
          >
            {t("poolGuide.breadcrumb.addressSpace")}
          </Link>
        </li>
        <li>
          <span aria-current="page">{t("poolGuide.breadcrumb.guide")}</span>
        </li>
      </ol>
    </nav>
  );
}

interface PoolGuidePageProps {
  envId: string;
  // The tab the address names; absent means the one that suits the environment's type.
  part?: GuidePart;
}

// PoolGuidePage is the Pool guidance page (D-23): what to free up first, the right swarm init for a
// new swarm, the rebuild runbook for an existing one and the daemon.json pools for a standalone
// host. It reads and explains; it never runs a command, sends anything to the server or puts what
// was typed in an address. The pool inputs of New swarm and Rebuild are one state, held here.
export function PoolGuidePage({ envId, part }: PoolGuidePageProps) {
  const t = useT();
  const allowed = usePermission("networks.read", envId);
  const environments = useQuery({ ...environmentsQuery, enabled: allowed });
  const [inputs, setInputs] = useState<SwarmInputs>(DEFAULT_SWARM_INPUTS);
  if (!allowed) return <NoPermission />;

  const envType = environments.data?.items?.find((e) => e.id === envId)?.envType;
  // A standalone host opens on its own part, anything else on the free-space tips. With no part in
  // the address the page waits for the environment's type rather than showing one tab and then
  // another.
  const waiting = part === undefined && environments.isPending;
  const active: GuidePart = part ?? (envType === "standalone" ? "standalone" : "free-space");

  function change(patch: Partial<SwarmInputs>) {
    setInputs((prev) => ({ ...prev, ...patch }));
  }

  return (
    <div className="mx-auto flex w-full max-w-[1152px] flex-col gap-4">
      <Breadcrumb envId={envId} />
      <header className="flex flex-col gap-1">
        <h1 className="text-2xl font-semibold">{t("poolGuide.title")}</h1>
        <p className="max-w-prose text-sm text-muted-foreground">{t("poolGuide.caption")}</p>
      </header>
      <Alert role="note">
        <Info aria-hidden="true" />
        <AlertDescription>{t("poolGuide.note")}</AlertDescription>
      </Alert>
      <Tabs value="" className="gap-4">
        <TabsList
          variant="line"
          aria-label={t("poolGuide.tabs.label")}
          className="h-9 max-w-full overflow-x-auto max-md:h-11"
        >
          {GUIDE_PARTS.map((p) => (
            <TabsTrigger
              key={p}
              value={p}
              asChild
              className="data-active:text-accent-ink after:bg-accent-ink max-md:h-11"
            >
              <Link
                to="/env/$envId/address-space/guide"
                params={{ envId }}
                search={{ part: p }}
                replace
                data-active={!waiting && p === active ? true : undefined}
                aria-current={!waiting && p === active ? "page" : undefined}
              >
                {t(TAB_KEYS[p])}
              </Link>
            </TabsTrigger>
          ))}
        </TabsList>
      </Tabs>
      {waiting ? (
        <Skeleton className="h-40 w-full" aria-busy="true" />
      ) : (
        <section aria-labelledby="pool-guide-heading" className="flex flex-col gap-4">
          <h2 id="pool-guide-heading" className="text-lg font-semibold">
            {t(TAB_KEYS[active])}
          </h2>
          {active === "new-swarm" ? <NewSwarmForm inputs={inputs} onChange={change} /> : null}
        </section>
      )}
    </div>
  );
}
