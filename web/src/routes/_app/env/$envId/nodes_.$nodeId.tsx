// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { createFileRoute, Outlet } from "@tanstack/react-router";

// A node's page. The trailing underscore on the file name keeps the page outside the list route,
// so it replaces the list instead of rendering under it. The frame comes with the page itself.
export const Route = createFileRoute("/_app/env/$envId/nodes_/$nodeId")({
  component: Outlet,
});
