// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { createFileRoute } from "@tanstack/react-router";

import { BrowsePage, browseSearchSchema } from "@/features/tags/browse-page";

// The trailing underscore on the file name keeps the page outside the Registries route, so it
// replaces that page and does not render under it.
function Browse() {
  return <BrowsePage search={Route.useSearch()} />;
}

export const Route = createFileRoute("/_app/registries_/browse")({
  validateSearch: browseSearchSchema,
  component: Browse,
});
