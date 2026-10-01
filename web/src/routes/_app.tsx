// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { createFileRoute, Outlet, redirect, useRouter } from "@tanstack/react-router";
import { useEffect, useState } from "react";

import { setCsrfToken, setUnauthorizedHandler } from "@/api/client";
import { loadInstance } from "@/api/instance";
import { queryClient } from "@/api/query-client";
import { AppSidebar } from "@/components/app-sidebar";
import { SidebarProvider, SidebarTrigger, useSidebar } from "@/components/ui/sidebar";
import { TooltipProvider } from "@/components/ui/tooltip";
import { UserMenu } from "@/components/user-menu";
import { VersionFooter } from "@/components/version-footer";
import { meQueryOptions, UnauthenticatedError, useMe } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { getSidebarOpen, setSidebarOpen } from "@/lib/sidebar-state";

// ShellBody is everything right of the sidebar: the 48px header, the page and,
// when the sidebar is not showing it, the version footer (PLAT-05).
function ShellBody() {
  const t = useT();
  const { data: me } = useMe();
  const { state, isMobile, openMobile } = useSidebar();
  // The mirror image of AppSidebar: the sidebar holds the footer while it is
  // expanded or its drawer is open, so this holds it the rest of the time.
  const footerHere = isMobile ? !openMobile : state === "collapsed";

  return (
    <div className="flex min-h-svh min-w-0 flex-1 flex-col bg-background">
      <header className="flex h-12 shrink-0 items-center gap-2 border-b px-4 md:px-6">
        <SidebarTrigger aria-label={t("shell.toggleSidebar")} className="max-md:size-11" />
        {/* The page title goes here: breadcrumbs or a heading, set by each page. */}
        <div id="header-title" className="min-w-0 flex-1 truncate text-sm font-medium" />
        {me ? <UserMenu username={me.user.username} role={me.user.role} /> : null}
      </header>
      <main id="main" tabIndex={-1} className="flex-1 p-4 outline-none md:p-6">
        <Outlet />
      </main>
      {footerHere ? (
        <div className="border-t px-4 py-2 md:px-6">
          <VersionFooter />
        </div>
      ) : null}
    </div>
  );
}

function AppLayout() {
  const t = useT();
  const router = useRouter();
  const [open, setOpen] = useState(getSidebarOpen);

  // A 401 on any later signed-in request means the session ended (expired,
  // revoked or signed out elsewhere): drop what is cached for it and go to
  // sign-in, which brings the browser back to this page afterwards. One-shot,
  // so a burst of failing requests navigates once.
  useEffect(() => {
    setUnauthorizedHandler((path) => {
      setUnauthorizedHandler(null);
      setCsrfToken(null);
      queryClient.removeQueries({ queryKey: meQueryOptions.queryKey });
      void router.navigate({
        to: "/login",
        search: { next: path === "/" ? undefined : path, expired: true },
        replace: true,
      });
    });
    return () => setUnauthorizedHandler(null);
  }, [router]);

  return (
    <TooltipProvider>
      <a
        href="#main"
        className="sr-only z-50 rounded-md bg-background px-3 py-2 text-sm text-foreground focus:not-sr-only focus:fixed focus:top-2 focus:left-2 focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
      >
        {t("shell.skipLink")}
      </a>
      <SidebarProvider
        open={open}
        onOpenChange={(next) => {
          setOpen(next);
          setSidebarOpen(next);
        }}
      >
        <AppSidebar />
        <ShellBody />
      </SidebarProvider>
    </TooltipProvider>
  );
}

export const Route = createFileRoute("/_app")({
  // Every signed-in page needs to know who is signed in. A fresh install goes
  // to the wizard first, and a visitor without a session goes to sign-in with
  // the page they asked for remembered in `next`.
  beforeLoad: async ({ location }) => {
    const instance = await loadInstance();
    if (instance.needsSetup) throw redirect({ to: "/setup" });
    try {
      await queryClient.ensureQueryData(meQueryOptions);
    } catch (err) {
      if (err instanceof UnauthenticatedError) {
        throw redirect({
          to: "/login",
          search: { next: location.href === "/" ? undefined : location.href },
        });
      }
      throw err;
    }
  },
  component: AppLayout,
});
