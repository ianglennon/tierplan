// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Server } from "lucide-react";

import {
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarGroup,
  SidebarHeader,
  useSidebar,
} from "@/components/ui/sidebar";
import { VersionFooter } from "@/components/version-footer";
import { useT } from "@/i18n/use-t";

// AppSidebar is the left navigation (D-53): the environment switcher on top,
// the selected environment's sections, global items at the bottom, and the
// version footer. It collapses to a 48px icon rail and is a drawer under
// 768px.
//
// The version footer is in the sidebar only while the sidebar is expanded or
// its mobile drawer is open. Otherwise the app layout renders it at the bottom
// of the page, so exactly one is visible at every size and state (PLAT-05).
export function AppSidebar() {
  const t = useT();
  const { state, isMobile } = useSidebar();
  const footerHere = isMobile || state === "expanded";

  return (
    <Sidebar
      collapsible="icon"
      mobileTitle={t("shell.sidebar.title")}
      mobileDescription={t("shell.sidebar.description")}
    >
      <SidebarHeader className="h-12 justify-center border-b border-sidebar-border p-2">
        {/* Plan 19 replaces this with the environment switcher. */}
        <div className="flex h-8 items-center gap-2 overflow-hidden px-2 text-sm text-muted-foreground max-md:h-11">
          <Server className="size-4 shrink-0" aria-hidden="true" />
          <span className="truncate">{t("shell.switcher.none")}</span>
        </div>
      </SidebarHeader>
      {/* Plans 19 and 20 add the environment's Dashboard and Resources here. */}
      <SidebarContent />
      {/* Settings and Audit are added by plans 23 and 25, once those routes exist
          for the typed links to point at. */}
      <SidebarGroup className="mt-auto p-0" />
      <SidebarFooter className="border-t border-sidebar-border px-4 py-2">
        {footerHere ? <VersionFooter /> : null}
      </SidebarFooter>
    </Sidebar>
  );
}
