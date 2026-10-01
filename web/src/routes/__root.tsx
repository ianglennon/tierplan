// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { createRootRoute, Outlet } from "@tanstack/react-router";

import { NotFound } from "@/components/not-found";

export const Route = createRootRoute({
  component: Outlet,
  notFoundComponent: NotFound,
});
