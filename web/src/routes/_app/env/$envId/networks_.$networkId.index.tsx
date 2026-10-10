// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { createFileRoute, redirect } from "@tanstack/react-router";

// The start address of a network's page. Until the Overview tab exists it opens the
// Attachments tab.
export const Route = createFileRoute("/_app/env/$envId/networks_/$networkId/")({
  beforeLoad: ({ params, search }) => {
    throw redirect({
      to: "/env/$envId/networks/$networkId/attachments",
      params,
      search,
      replace: true,
    });
  },
});
