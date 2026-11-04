// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { environmentsQuery } from "@/api/environments";
import { useFleetSummary } from "@/api/fleet";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { EnvFilter } from "@/features/fleet/env-filter";
import { FleetCoverageAlert } from "@/features/fleet/fleet-coverage-alert";
import { FleetFailingTable } from "@/features/fleet/fleet-failing-table";
import { FLEET_TABS, type FleetTab } from "@/features/fleet/fleet-search";
import { useT } from "@/i18n/use-t";

interface FleetTabsProps {
  tab: FleetTab;
  // The environments the fleet tabs are narrowed to.
  env: readonly string[];
  onTabChange: (tab: FleetTab) => void;
  onEnvChange: (ids: string[]) => void;
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
        <FleetSummaryTab env={env} onEnvChange={onEnvChange} />
      </TabsContent>
      <TabsContent value="pools" className="flex flex-col gap-4">
        <FleetSummaryTab env={env} onEnvChange={onEnvChange} />
      </TabsContent>
    </Tabs>
  );
}

// The Updates and Address pools tabs open with what every fleet tab opens with: the environments
// the view leaves out, and the environment filter. Their tables are the next plan's (12-29).
function FleetSummaryTab({
  env,
  onEnvChange,
}: {
  env: readonly string[];
  onEnvChange: (ids: string[]) => void;
}) {
  const summary = useFleetSummary();
  const environments = useQuery(environmentsQuery);
  return (
    <>
      <FleetCoverageAlert notIncluded={summary.data?.notIncluded} />
      <div className="flex flex-wrap items-center gap-2">
        <EnvFilter
          value={env}
          onChange={onEnvChange}
          environments={(environments.data?.items ?? []).map((e) => ({ id: e.id, name: e.name }))}
        />
      </div>
    </>
  );
}
