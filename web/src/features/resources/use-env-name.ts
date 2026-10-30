// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useQuery } from "@tanstack/react-query";

import { environmentsQuery } from "@/api/environments";
import { useT } from "@/i18n/use-t";

// useEnvName is the display name of an environment for copy such as "Nothing
// to show on {name} right now.", read from the list the switcher already
// loaded, with a generic phrase while it is still on its way.
export function useEnvName(envId: string): string {
  const t = useT();
  const { data } = useQuery(environmentsQuery);
  return data?.items?.find((e) => e.id === envId)?.name ?? t("dashboard.environmentFallback");
}
