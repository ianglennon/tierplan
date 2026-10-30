// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { createRootRoute, Outlet } from "@tanstack/react-router";

import { NotFound } from "@/components/not-found";
import { RouteError } from "@/components/route-error";
import { Toaster } from "@/components/ui/sonner";

// RootLayout renders the page and the app's only toast region. It lives here,
// not in the signed-in shell, so toasts also work on pages outside it: the
// wizard's last step (/welcome) and sign-in. Do not mount a second Toaster.
function RootLayout() {
  return (
    <>
      <Outlet />
      <Toaster />
    </>
  );
}

export const Route = createRootRoute({
  component: RootLayout,
  notFoundComponent: NotFound,
  // WR-18: every route without its own error component falls back here, so a
  // failed guard or loader shows a translated page with Retry and the version
  // footer instead of TanStack's raw error screen.
  errorComponent: RouteError,
});
