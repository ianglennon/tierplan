// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { createRootRoute, Outlet } from "@tanstack/react-router";

import { NotFound } from "@/components/not-found";
import { RouteError } from "@/components/route-error";

export const Route = createRootRoute({
  component: Outlet,
  notFoundComponent: NotFound,
  // WR-18: every route without its own error component falls back here, so a
  // failed guard or loader shows a translated page with Retry and the version
  // footer instead of TanStack's raw error screen.
  errorComponent: RouteError,
});
