// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { environmentsQuery } from "@/api/environments";
import { type FleetShowValue, useFleetPools } from "@/api/fleet";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { EnvFilter } from "@/features/fleet/env-filter";
import { FleetFailingTable } from "@/features/fleet/fleet-failing-table";
import { FleetPoolsTable } from "@/features/fleet/fleet-pools-table";
import { FLEET_TABS, type FleetTab } from "@/features/fleet/fleet-search";
import { FleetUpdatesTab } from "@/features/fleet/fleet-updates-tab";
import { useT } from "@/i18n/use-t";

interface FleetTabsProps {
  tab: FleetTab;
  // The environments the fleet tabs are narrowed to.
  env: readonly string[];
  onTabChange: (tab: FleetTab) => void;
  onEnvChange: (ids: string[]) => void;
  // The Updates tab's Show chip: the URL's, with "updates" as the default.
  show: FleetShowValue;
  onShowChange: (show: FleetShowValue) => void;
  // The Environments tab's body: today's overview table, unchanged.
  environmentsBody: ReactNode;
}

// FleetTabs is the Overview's tab list and the body of the tab that is showing (D-17). The tab
// is the URL's: the route owns it and is told when a trigger is pressed. Only the showing tab's
// body is mounted, so a hidden tab reads nothing.
export function FleetTabs({
  tab,
  env,
  onTabChange,
  onEnvChange,
  show,
  onShowChange,
  environmentsBody,
}: FleetTabsProps) {
  const t = useT();
  return (
    <Tabs
      value={tab}
      onValueChange={(next) => {
        const known = FLEET_TABS.find((f) => f === next);
        if (known) onTabChange(known);
      }}
      className="gap-4"
    >
      <TabsList
        variant="line"
        aria-label={t("fleet.tabs.label")}
        className="max-w-full justify-start overflow-x-auto max-md:h-11"
      >
        {FLEET_TABS.map((name) => (
          <TabsTrigger key={name} value={name} className="max-md:h-11 max-md:px-3">
            {t(`fleet.tabs.${name}`)}
          </TabsTrigger>
        ))}
      </TabsList>
      <TabsContent value="environments" className="flex flex-col gap-4">
        {environmentsBody}
      </TabsContent>
      <TabsContent value="failing" className="flex flex-col gap-4">
        <FleetFailingTable env={env} onEnvChange={onEnvChange} />
      </TabsContent>
      <TabsContent value="updates" className="flex flex-col gap-4">
        <FleetUpdatesTab
          env={env}
          show={show}
          onEnvChange={onEnvChange}
          onShowChange={onShowChange}
        />
      </TabsContent>
      <TabsContent value="pools" className="flex flex-col gap-4">
        <FleetPoolsTab env={env} onEnvChange={onEnvChange} />
      </TabsContent>
    </Tabs>
  );
}

// FleetPoolsTab is the Address pools tab: the environment filter, then the pools near full of the
// environments that were read, or why they could not be read.
function FleetPoolsTab({
  env,
  onEnvChange,
}: {
  env: readonly string[];
  onEnvChange: (ids: string[]) => void;
}) {
  const t = useT();
  const query = useFleetPools(env);
  const environments = useQuery(environmentsQuery);
  const data = query.data;
  const failed = query.isError && !data;
  return (
    <>
      <div className="flex flex-wrap items-center gap-2">
        <EnvFilter
          value={env}
          onChange={onEnvChange}
          environments={(environments.data?.items ?? []).map((e) => ({ id: e.id, name: e.name }))}
        />
      </div>
      {failed ? (
        <Alert variant="destructive">
          <AlertTitle>{t("fleet.pools.error.heading")}</AlertTitle>
          <AlertDescription className="flex flex-wrap items-center gap-3">
            <span>{t("fleet.pools.error.body")}</span>
            <Button
              variant="outline"
              size="sm"
              disabled={query.isFetching}
              onClick={() => void query.refetch()}
            >
              {query.isFetching ? <Spinner aria-hidden="true" /> : null}
              {t("fleet.pools.error.retry")}
            </Button>
          </AlertDescription>
        </Alert>
      ) : null}
      {!data && !failed ? (
        <div className="flex flex-col gap-2" role="status" aria-label={t("fleet.pools.loading")}>
          {Array.from({ length: 6 }, (_, i) => (
            // biome-ignore lint/suspicious/noArrayIndexKey: placeholder rows have no identity
            <Skeleton key={i} className="h-9 w-full" />
          ))}
        </div>
      ) : null}
      {data ? <FleetPoolsTable items={data.items ?? []} notIncluded={data.notIncluded} /> : null}
    </>
  );
}
