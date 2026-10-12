// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { createFileRoute } from "@tanstack/react-router";

import {
  RegistriesPage,
  type RegistriesSearchPatch,
  registriesSearchSchema,
} from "@/features/registries/registries-page";

function Registries() {
  const search = Route.useSearch();
  const navigate = Route.useNavigate();

  // A change to the search or the sheet replaces the history entry, so the back button
  // leaves the page and does not walk through every key.
  function change(patch: RegistriesSearchPatch) {
    void navigate({
      search: (prev) => {
        const next: Record<string, unknown> = { ...prev, ...patch };
        for (const key of Object.keys(next)) if (next[key] === undefined) delete next[key];
        return next;
      },
      replace: true,
    });
  }

  return <RegistriesPage search={search} onSearchChange={change} />;
}

export const Route = createFileRoute("/_app/registries")({
  validateSearch: registriesSearchSchema,
  component: Registries,
});
