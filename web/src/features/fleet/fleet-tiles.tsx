// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import type { FleetSummary } from "@/api/fleet";
import { CountTile } from "@/components/count-tile";
import { useNow } from "@/components/relative-time";
import type { FleetTab } from "@/features/fleet/fleet-search";
import { useT } from "@/i18n/use-t";
import { formatRelative } from "@/lib/format";

interface FleetTilesProps {
  // Undefined until the first answer. With loading the values are skeletons at their final height.
  summary?: FleetSummary;
  loading?: boolean;
  // The summary could not be read: every tile shows a dash and says so.
  failed?: boolean;
  // The environments the fleet tabs are narrowed to: a tile's link keeps them.
  env?: readonly string[];
}

// The Overview's four tiles (D-17, UI-SPEC "Overview: fleet tiles and tabs"). Each is a link to
// its tab and keeps the environments the view is narrowed to. Two columns under 1024px, four
// from there, 16px apart.
export function FleetTiles({ summary, loading, failed, env }: FleetTilesProps) {
  const t = useT();
  const now = useNow();
  const waiting = loading && !summary;
  // A count that is not known shows a dash, never a 0 (12-26): the server sends null for it, and a
  // 0 beside an environment that was not read is as unknown as a null.
  const missing = (summary?.notIncluded?.length ?? 0) > 0;
  const known = (count: number | null | undefined): number | undefined =>
    count === null || count === undefined || (count === 0 && missing) ? undefined : count;
  const unread = failed || !summary;
  const unreadLine = missing && !failed ? t("fleet.tiles.partial") : t("fleet.tiles.unreadable");

  const environments = summary?.environments;
  const failing = known(summary?.failing.count);
  const updates = known(summary?.updates.count);
  const pools = known(summary?.pools.nearFull);

  const tab = (to: FleetTab) => ({
    to: "/environments" as const,
    search: { tab: to, env: env && env.length > 0 ? [...env] : undefined },
  });

  let environmentsLine: string | undefined;
  if (environments) {
    environmentsLine =
      environments.needAttention > 0
        ? t("fleet.tiles.environments.needAttention", { count: environments.needAttention })
        : t("fleet.tiles.environments.allReporting");
  }

  let failingLine: string | undefined;
  if (failing !== undefined) {
    failingLine =
      failing > 0 && summary?.failing.environments
        ? t("fleet.tiles.failing.in", { count: summary.failing.environments })
        : t("fleet.tiles.failing.none");
  }

  let updatesLine: string | undefined;
  if (updates !== undefined && summary) {
    const u = summary.updates;
    if (u.checksFailed !== null && u.checksFailed > 0) {
      updatesLine = t("fleet.tiles.updates.checksFailed", { count: u.checksFailed });
    } else if (u.allChecked && u.lastCheckedAt) {
      updatesLine = t("fleet.tiles.updates.checked", {
        when: formatRelative(u.lastCheckedAt, new Date(now), t),
      });
    } else {
      updatesLine = t("fleet.tiles.updates.notAllChecked");
    }
  }

  let poolsLine: string | undefined;
  if (pools !== undefined && summary) {
    const almost = summary.pools.almostFull ?? 0;
    poolsLine =
      almost > 0
        ? t("fleet.tiles.pools.almostFull", { count: almost })
        : pools === 0
          ? t("fleet.tiles.pools.allUnder")
          : t("fleet.tiles.pools.noneAlmost");
  }

  return (
    <section aria-label={t("fleet.tiles.label")} className="grid grid-cols-2 gap-4 lg:grid-cols-4">
      <CountTile
        {...tab("environments")}
        label={t("fleet.tiles.environments.label")}
        value={unread ? undefined : environments?.total}
        subline={unread ? unreadLine : environmentsLine}
        loading={waiting}
      />
      <CountTile
        {...tab("failing")}
        label={t("fleet.tiles.failing.label")}
        value={unread ? undefined : failing}
        subline={unread || failing === undefined ? unreadLine : failingLine}
        loading={waiting}
        dot="offline"
      />
      <CountTile
        {...tab("updates")}
        label={t("fleet.tiles.updates.label")}
        value={unread ? undefined : updates}
        subline={unread || updates === undefined ? unreadLine : updatesLine}
        loading={waiting}
      />
      <CountTile
        {...tab("pools")}
        label={t("fleet.tiles.pools.label")}
        value={unread ? undefined : pools}
        subline={unread || pools === undefined ? unreadLine : poolsLine}
        loading={waiting}
      />
    </section>
  );
}
