// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { CountTile } from "@/components/count-tile";
import { useT } from "@/i18n/use-t";

interface UpdatesTileProps {
  envId: string;
  // How many services and containers have an update available, from the summary. Absent when the
  // server could not read it: the tile then says so rather than showing a 0 that means "none".
  value?: number;
  // The summary is still loading.
  loading?: boolean;
}

// UpdatesTile is the environment dashboard's "Updates available" tile (IMG-05). It links to the
// Updates tab of the environments overview, filtered to this environment. The count is the
// server's, and a count that could not be read is a dash with a line saying so, never 0.
export function UpdatesTile({ envId, value, loading }: UpdatesTileProps) {
  const t = useT();
  const unread = !loading && value === undefined;
  return (
    <CountTile
      label={t("updates.tile.label")}
      value={value}
      subline={unread ? t("updates.tile.error") : t("updates.tile.subline")}
      to="/environments"
      // The link is /environments?tab=updates&env={id}. The environments route does not take a tab yet (the Updates tab arrives with the Overview
      // tabs); until then it drops the key and opens its default view, so the link is safe now.
      search={{ tab: "updates", env: envId } as never}
      loading={loading}
    />
  );
}
