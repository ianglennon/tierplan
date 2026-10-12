// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { useMemo } from "react";
import { environmentsQuery } from "@/api/environments";
import { credentialsQuery } from "@/api/registries";
import { NoPermission } from "@/components/full-page-state";
import { MAX_QUERY } from "@/features/containers/list-search";
import { CredentialTable } from "@/features/registries/credential-table";
import { useAnyEnvironmentPermission, useDocumentTitle, usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { z } from "@/lib/zod-config";

// The longest value a search parameter holds that is an ID: a UUID is 36.
const MAX_ID = 64;

// The route's search (UI-SPEC "Screens"): the search text, the Scope filter ("instance"
// or an environment's ID) and the credential whose sheet is open ("new" for the add
// form, an ID for a credential). A value that does not fit is dropped, so a stale or
// hand-edited link shows the default list instead of an error.
export const registriesSearchSchema = z.object({
  q: z.string().min(1).max(MAX_QUERY).optional().catch(undefined),
  scope: z.string().min(1).max(MAX_ID).optional().catch(undefined),
  credential: z.string().min(1).max(MAX_ID).optional().catch(undefined),
});

export type RegistriesSearch = z.output<typeof registriesSearchSchema>;

// A change to the search: a key set to undefined leaves the URL.
export type RegistriesSearchPatch = {
  [K in keyof RegistriesSearch]?: RegistriesSearch[K] | undefined;
};

interface RegistriesPageProps {
  search: RegistriesSearch;
  onSearchChange: (patch: RegistriesSearchPatch) => void;
}

// RegistriesPage is the credential list everyone who reads images can open (D-09). The
// server limits the rows to what the caller may see; this only decides what to show.
// Adding is for an instance administrator (registries.instance), and everyone else
// sees the same table read-only.
export function RegistriesPage({ search, onSearchChange }: RegistriesPageProps) {
  const t = useT();
  useDocumentTitle(t("registries.title"));
  const allowed = useAnyEnvironmentPermission("images.read");
  const canAdd = usePermission("registries.instance");
  const credentials = useQuery({ ...credentialsQuery, enabled: allowed });
  const environments = useQuery({ ...environmentsQuery, enabled: allowed });

  const scopes = useMemo(
    () =>
      (environments.data?.items ?? [])
        .map((e) => ({ id: e.id, name: e.name }))
        .sort((a, b) => a.name.localeCompare(b.name)),
    [environments.data],
  );

  if (!allowed) return <NoPermission />;

  return (
    <CredentialTable
      credentials={credentials.data}
      isPending={credentials.isPending}
      isError={credentials.isError}
      isFetching={credentials.isFetching}
      refetch={credentials.refetch}
      filters={{ q: search.q, scope: search.scope }}
      onFiltersChange={onSearchChange}
      scopes={scopes}
      canAdd={canAdd}
      onAdd={() => onSearchChange({ credential: "new" })}
      onOpen={(id) => onSearchChange({ credential: id })}
    />
  );
}
