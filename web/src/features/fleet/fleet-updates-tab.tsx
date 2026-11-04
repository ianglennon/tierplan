// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import { RefreshCw } from "lucide-react";
import { useMemo } from "react";
import { Trans } from "react-i18next";

import { environmentsQuery } from "@/api/environments";
import {
  type FleetSelected,
  type FleetShowValue,
  useFleetCheck,
  useFleetChecking,
  useFleetUpdates,
} from "@/api/fleet";
import { useImageUpdateJobs } from "@/api/updates";
import { RelativeTime } from "@/components/relative-time";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group";
import { EnvFilter } from "@/features/fleet/env-filter";
import { FleetBulkBar } from "@/features/fleet/fleet-bulk-bar";
import { FleetCoverageAlert } from "@/features/fleet/fleet-coverage-alert";
import { FleetEmptyUpdates } from "@/features/fleet/fleet-empty-updates";
import { FLEET_SHOWS } from "@/features/fleet/fleet-search";
import { itemKey, UpdatesTable } from "@/features/fleet/updates-table";
import { useChosenKeys } from "@/features/resources/selection";
import { useT } from "@/i18n/use-t";

interface FleetUpdatesTabProps {
  // The environments the tab is narrowed to (empty: all the caller may read).
  env: readonly string[];
  show: FleetShowValue;
  onEnvChange: (ids: string[]) => void;
  onShowChange: (show: FleetShowValue) => void;
}

// FleetUpdatesTab is the Updates tab of the Overview (ENV-10, IMG-05, D-17): the coverage alert, a
// toolbar (the environment filter, the Show chips, Check now and when the last check was) and the
// table of every pending update the caller may read, grouped by environment. It says what it could
// not see, and its empty states claim "up to date" only when every check that counts succeeded.
export function FleetUpdatesTab({ env, show, onEnvChange, onShowChange }: FleetUpdatesTabProps) {
  const t = useT();
  // While an update is running the tab reads again every few seconds, so its rows go from Queued to
  // Updating… to their end as they happen.
  const updateJobs = useImageUpdateJobs();
  const busy = updateJobs.data?.some((j) => j.state === "running") ?? false;
  const query = useFleetUpdates(env, show, busy);
  const environments = useQuery(environmentsQuery);
  const check = useFleetCheck();
  const checking = useFleetChecking();
  // The rows ticked for a bulk update, as keys. The table works out which rows can be ticked; the bar
  // reads the workloads they stand for.
  const chosen = useChosenKeys<string>();

  const data = query.data;
  const failed = query.isError && !data;
  const loading = !data && !failed;
  const groups = data?.groups ?? [];

  // The ticked workloads, from the rows as they are now: one that has since stopped being updatable is
  // not among them, and the request is built from these, not from the rows on screen.
  const selected = useMemo(() => {
    const out: FleetSelected[] = [];
    for (const group of groups) {
      for (const item of group.items ?? []) {
        if (item.selectable && chosen[0].has(itemKey(group.envId, item))) {
          out.push({ envId: group.envId, envName: group.envName, type: group.type, item });
        }
      }
    }
    return out;
  }, [groups, chosen]);
  // The environments that did not report: nothing in them can be updated now.
  const notReporting = useMemo(
    () =>
      new Set(
        (data?.notIncluded ?? [])
          .filter((n) => n.reason !== "nodes_not_reporting")
          .map((n) => n.envId),
      ),
    [data?.notIncluded],
  );

  const label = (value: FleetShowValue) => {
    const word = t(`fleet.updates.show.${value}`);
    if (!data || value === "all") return word;
    return t("fleet.updates.show.counted", { label: word, count: data.counts[value] });
  };

  return (
    <>
      {/* The empty state shows the alert itself, next to the words it qualifies. */}
      {data && groups.length === 0 ? null : <FleetCoverageAlert notIncluded={data?.notIncluded} />}
      <div className="flex flex-wrap items-center gap-2">
        <EnvFilter
          value={env}
          onChange={onEnvChange}
          environments={(environments.data?.items ?? []).map((e) => ({ id: e.id, name: e.name }))}
        />
        <ToggleGroup
          type="single"
          variant="outline"
          size="sm"
          value={show}
          aria-label={t("fleet.updates.show.label")}
          onValueChange={(next) => {
            // Pressing the chip that is on turns it off; one chip is always on.
            const known = FLEET_SHOWS.find((s) => s === next);
            if (known) onShowChange(known);
          }}
        >
          {FLEET_SHOWS.map((value) => (
            <ToggleGroupItem key={value} value={value} className="max-md:h-11">
              {label(value)}
            </ToggleGroupItem>
          ))}
        </ToggleGroup>
        {selected.length > 0 ? null : (
          <Button
            variant="outline"
            size="sm"
            className="ms-auto max-md:h-11"
            aria-busy={checking || undefined}
            onClick={() => {
              if (checking) return;
              check.mutate(env);
            }}
          >
            {checking ? <Spinner aria-hidden="true" /> : <RefreshCw aria-hidden="true" />}
            {checking ? t("fleet.updates.checking") : t("fleet.updates.checkNow")}
          </Button>
        )}
      </div>
      {selected.length > 0 ? (
        <FleetBulkBar
          items={selected}
          notReporting={notReporting}
          onClear={() => chosen[1](new Set())}
        />
      ) : null}
      {data ? (
        <p className="text-xs text-muted-foreground">
          {data.coverage.lastCheckedAt ? (
            <Trans
              i18nKey="fleet.updates.lastChecked"
              ns="fleet"
              count={data.coverage.environments}
              components={{ time: <RelativeTime value={data.coverage.lastCheckedAt} /> }}
            />
          ) : (
            t("fleet.updates.neverChecked")
          )}
        </p>
      ) : null}
      {failed ? (
        <Alert variant="destructive">
          <AlertTitle>{t("fleet.updates.error.heading")}</AlertTitle>
          <AlertDescription className="flex flex-wrap items-center gap-3">
            <span>{t("fleet.updates.error.body")}</span>
            <Button
              variant="outline"
              size="sm"
              disabled={query.isFetching}
              onClick={() => void query.refetch()}
            >
              {query.isFetching ? <Spinner aria-hidden="true" /> : null}
              {t("fleet.updates.error.retry")}
            </Button>
          </AlertDescription>
        </Alert>
      ) : null}
      {loading ? (
        <div className="flex flex-col gap-2" role="status" aria-label={t("fleet.updates.loading")}>
          {Array.from({ length: 6 }, (_, i) => (
            // biome-ignore lint/suspicious/noArrayIndexKey: placeholder rows have no identity
            <Skeleton key={i} className="h-9 w-full" />
          ))}
        </div>
      ) : null}
      {data && groups.length > 0 ? (
        <UpdatesTable groups={groups} show={show} chosen={chosen} />
      ) : null}
      {data && groups.length === 0 ? (
        <FleetEmptyUpdates
          coverage={data.coverage}
          notIncluded={data.notIncluded}
          show={show}
          onShow={onShowChange}
        />
      ) : null}
    </>
  );
}
