// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { createFileRoute, Outlet } from "@tanstack/react-router";
import { useState } from "react";

import { AppSidebar } from "@/components/app-sidebar";
import { SidebarProvider, SidebarTrigger, useSidebar } from "@/components/ui/sidebar";
import { TooltipProvider } from "@/components/ui/tooltip";
import { UserMenu } from "@/components/user-menu";
import { VersionFooter } from "@/components/version-footer";
import { DataNotPersistedBanner, HealthBell } from "@/components/warnings";
import { EnvPalette } from "@/features/environments/env-palette";
import { EnvHealthStreamProvider } from "@/features/environments/health-stream";
import { PullsIndicator } from "@/features/pulls/pulls-indicator";
import { useAccessRefetch } from "@/hooks/use-access-refetch";
import { useAnyEnvironmentPermission, useMe, usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { requireSession, useSessionExpiry } from "@/lib/require-session";
import { getSidebarOpen, setSidebarOpen } from "@/lib/sidebar-state";

// ShellBody is everything right of the sidebar: the 48px header, the page and,
// when the sidebar is not showing it, the version footer (PLAT-05).
function ShellBody() {
  const t = useT();
  const { data: me } = useMe();
  // Instance warnings are for admins: the server refuses them to anyone else.
  const canSeeWarnings = usePermission("settings.read");
  // The bell also carries the problems of the environments the caller may read (D-39),
  // which the server filters to them, so anyone who may read one gets the bell.
  const readsEnvironments = useAnyEnvironmentPermission("environments.read");
  const canSeeBell = canSeeWarnings || readsEnvironments;
  const { state, isMobile, openMobile } = useSidebar();
  // The mirror image of AppSidebar: the sidebar holds the footer while it is
  // expanded or its drawer is open, so this holds it the rest of the time.
  const footerHere = isMobile ? !openMobile : state === "collapsed";

  return (
    <div className="flex min-h-svh min-w-0 flex-1 flex-col bg-background">
      {canSeeWarnings ? <DataNotPersistedBanner /> : null}
      <header className="flex h-12 shrink-0 items-center gap-2 border-b px-4 md:px-6">
        <SidebarTrigger aria-label={t("shell.toggleSidebar")} className="max-md:size-11" />
        {/* The page title goes here: breadcrumbs or a heading, set by each page. */}
        <div id="header-title" className="min-w-0 flex-1 truncate text-sm font-medium" />
        {me ? <PullsIndicator /> : null}
        {canSeeBell ? <HealthBell /> : null}
        {me ? <UserMenu username={me.user.username} instanceAdmin={me.user.instanceAdmin} /> : null}
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
  const [open, setOpen] = useState(getSidebarOpen);

  // A 403 or 404 on a later request can mean the user's access changed: read
  // /api/me again, at most every 5 seconds, and move them somewhere they can
  // still see (D-10).
  useAccessRefetch();

  // A 401 on any later signed-in request sends the browser to sign-in and back.
  useSessionExpiry();

  return (
    <TooltipProvider>
      <a
        href="#main"
        className="sr-only z-50 rounded-md bg-background px-3 py-2 text-sm text-foreground focus:not-sr-only focus:fixed focus:top-2 focus:left-2 focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
      >
        {t("shell.skipLink")}
      </a>
      {/* One health stream for the whole signed-in app (D-36): the overview,
          the switcher and the bell all read what it keeps current. */}
      <EnvHealthStreamProvider>
        <SidebarProvider
          open={open}
          onOpenChange={(next) => {
            setOpen(next);
            setSidebarOpen(next);
          }}
        >
          <AppSidebar />
          <ShellBody />
          {/* Cmd/Ctrl+K from any signed-in page (D-35). A code editor added
              later must set data-code-editor on its root, or the shortcut
              will open this over the editor's own use of the key. */}
          <EnvPalette />
        </SidebarProvider>
      </EnvHealthStreamProvider>
    </TooltipProvider>
  );
}

export const Route = createFileRoute("/_app")({
  // Every signed-in page needs to know who is signed in (see requireSession).
  beforeLoad: requireSession,
  component: AppLayout,
});
