// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useRouter } from "@tanstack/react-router";
import { KeyRound, LogOut } from "lucide-react";
import { useRef, useState } from "react";

import { api, setCsrfToken, setUnauthorizedHandler } from "@/api/client";
import { queryClient } from "@/api/query-client";
import { Avatar, AvatarFallback } from "@/components/ui/avatar";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { ChangePasswordDialog } from "@/features/account/change-password-dialog";
import { useT } from "@/i18n/use-t";
import { getTheme, setTheme, type ThemeMode } from "@/lib/theme";

interface UserMenuProps {
  username: string;
  role: string;
}

// Initials only (UI-SPEC): the first two characters of the username.
function initials(username: string): string {
  return [...username].slice(0, 2).join("").toUpperCase();
}

const THEMES: ReadonlyArray<{ mode: ThemeMode; key: string }> = [
  { mode: "light", key: "shell.userMenu.light" },
  { mode: "dark", key: "shell.userMenu.dark" },
  { mode: "system", key: "shell.userMenu.system" },
];

// UserMenu is the header menu on every signed-in page: who is signed in, the
// theme choice, Change password (D-34) and Log out (AUTH-03, D-63).
export function UserMenu({ username, role }: UserMenuProps) {
  const t = useT();
  const router = useRouter();
  const [theme, setThemeState] = useState<ThemeMode>(getTheme);
  const [changePasswordOpen, setChangePasswordOpen] = useState(false);
  const triggerRef = useRef<HTMLButtonElement>(null);

  function chooseTheme(value: string) {
    const mode = THEMES.find((th) => th.mode === value)?.mode;
    if (!mode) return;
    setTheme(mode);
    setThemeState(mode);
  }

  async function logout() {
    // The sign-out is deliberate, so a 401 on the way must not show the
    // "session expired" notice.
    setUnauthorizedHandler(null);
    try {
      await api.POST("/api/auth/logout");
    } catch {
      // Leave anyway. If the session survived, /login sends the browser back in.
    }
    setCsrfToken(null);
    await router.navigate({ to: "/login", replace: true });
    // Only now that the signed-in pages are gone: nothing they fetched stays
    // behind for the next person at this browser.
    queryClient.clear();
  }

  return (
    <>
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <Button
            ref={triggerRef}
            variant="ghost"
            size="icon"
            aria-label={t("shell.userMenu.trigger")}
          >
            <Avatar size="sm">
              <AvatarFallback>{initials(username)}</AvatarFallback>
            </Avatar>
          </Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end" className="w-56">
          <DropdownMenuLabel className="flex flex-col gap-0.5 font-normal">
            <span className="truncate text-sm font-medium text-foreground" title={username}>
              {username}
            </span>
            {role ? (
              <span className="text-xs text-muted-foreground">
                {role === "admin" ? t("shell.userMenu.roleAdmin") : role}
              </span>
            ) : null}
          </DropdownMenuLabel>
          <DropdownMenuSeparator />
          <DropdownMenuItem onSelect={() => setChangePasswordOpen(true)}>
            <KeyRound aria-hidden="true" />
            {t("shell.userMenu.changePassword")}
          </DropdownMenuItem>
          <DropdownMenuSeparator />
          <DropdownMenuLabel className="text-xs font-normal text-muted-foreground">
            {t("shell.userMenu.theme")}
          </DropdownMenuLabel>
          <DropdownMenuRadioGroup value={theme} onValueChange={chooseTheme}>
            {THEMES.map((th) => (
              <DropdownMenuRadioItem key={th.mode} value={th.mode}>
                {t(th.key)}
              </DropdownMenuRadioItem>
            ))}
          </DropdownMenuRadioGroup>
          <DropdownMenuSeparator />
          <DropdownMenuItem onSelect={logout}>
            <LogOut aria-hidden="true" />
            {t("shell.userMenu.logout")}
          </DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>
      {/* Outside the menu content, so closing the menu never unmounts the dialog. */}
      <ChangePasswordDialog
        open={changePasswordOpen}
        onOpenChange={setChangePasswordOpen}
        returnFocusRef={triggerRef}
      />
    </>
  );
}
