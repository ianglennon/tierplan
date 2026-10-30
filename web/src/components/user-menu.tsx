// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { KeyRound, LogOut, MonitorSmartphone, UserRound } from "lucide-react";
import { useRef, useState } from "react";

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
import { ProfileDialog } from "@/features/account/profile-dialog";
import { SessionsDialog } from "@/features/account/sessions-dialog";
import { useSignOut } from "@/features/account/sign-out";
import { useT } from "@/i18n/use-t";
import { getTheme, setTheme, type ThemeMode } from "@/lib/theme";

interface UserMenuProps {
  username: string;
  instanceAdmin: boolean;
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
// theme choice, Profile (D-24), Sessions (D-23), Change password (D-34) and Log out (AUTH-03, D-63).
export function UserMenu({ username, instanceAdmin }: UserMenuProps) {
  const t = useT();
  const signOut = useSignOut();
  const [theme, setThemeState] = useState<ThemeMode>(getTheme);
  const [changePasswordOpen, setChangePasswordOpen] = useState(false);
  const [profileOpen, setProfileOpen] = useState(false);
  const [sessionsOpen, setSessionsOpen] = useState(false);
  const triggerRef = useRef<HTMLButtonElement>(null);

  function chooseTheme(value: string) {
    const mode = THEMES.find((th) => th.mode === value)?.mode;
    if (!mode) return;
    setTheme(mode);
    setThemeState(mode);
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
            {instanceAdmin ? (
              <span className="text-xs text-muted-foreground">{t("shell.userMenu.roleAdmin")}</span>
            ) : null}
          </DropdownMenuLabel>
          <DropdownMenuSeparator />
          <DropdownMenuItem onSelect={() => setProfileOpen(true)}>
            <UserRound aria-hidden="true" />
            {t("sessions.menu.profile")}
          </DropdownMenuItem>
          <DropdownMenuItem onSelect={() => setSessionsOpen(true)}>
            <MonitorSmartphone aria-hidden="true" />
            {t("sessions.menu.sessions")}
          </DropdownMenuItem>
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
          <DropdownMenuItem onSelect={() => void signOut()}>
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
      <ProfileDialog open={profileOpen} onOpenChange={setProfileOpen} returnFocusRef={triggerRef} />
      <SessionsDialog
        open={sessionsOpen}
        onOpenChange={setSessionsOpen}
        returnFocusRef={triggerRef}
      />
    </>
  );
}
