// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { createFileRoute, redirect } from "@tanstack/react-router";

import { queryClient } from "@/api/query-client";
import { visibleSettingsTabs } from "@/features/settings/tabs";
import { meQueryOptions } from "@/hooks/use-me";

// /settings has no page of its own: it opens the first tab the user may see,
// the same list the tab bar shows (WR-19). A user with no Settings permission
// at all goes to the dashboard. The signed-in guard has already loaded the me
// query, so this reads it from the cache.
export const Route = createFileRoute("/_app/settings/")({
  beforeLoad: async () => {
    const me = await queryClient.ensureQueryData(meQueryOptions);
    const [tab] = visibleSettingsTabs(me);
    throw redirect({ to: tab?.to ?? "/", replace: true });
  },
});
