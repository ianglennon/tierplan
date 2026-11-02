// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import { TriangleAlert } from "lucide-react";
import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { TierPlanLogo } from "@/components/tierplan-logo";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { VersionFooter } from "@/components/version-footer";
import { useSignOut } from "@/features/account/sign-out";
import { meQueryOptions } from "@/hooks/use-me";
import { cn } from "@/lib/utils";

// Hosts where plain http: is not a network exposure (D-12).
const LOCAL_HOSTNAMES = new Set(["localhost", "127.0.0.1", "[::1]", "::1"]);

// isPlainHttpRemote is true when the page was loaded over http: from a host
// other than the machine itself, so typed passwords cross the network readable.
export function isPlainHttpRemote(
  loc: Pick<Location, "protocol" | "hostname"> = window.location,
): boolean {
  return loc.protocol === "http:" && !LOCAL_HOSTNAMES.has(loc.hostname);
}

interface AuthLayoutProps {
  title: string;
  subtitle?: string;
  wide?: boolean;
  // Pages a signed-in user can land on outside the shell (/welcome, not-found)
  // opt in, so they always offer a way to log out (W-3, AUTH-03).
  showSignOut?: boolean;
  children?: ReactNode;
}

// SignedInBar is "Signed in as {username}" and a Log out button. It reads the
// me query and renders nothing without an answer, so an anonymous visitor sees
// nothing; AuthLayout mounts it only for pages that opt in, so /login and
// /setup never make the request.
function SignedInBar() {
  const { t } = useTranslation();
  const signOut = useSignOut();
  const { data: me } = useQuery(meQueryOptions);
  if (!me) return null;
  return (
    <div className="flex items-center justify-end gap-2 text-sm text-muted-foreground">
      <span className="truncate" title={me.user.username}>
        {t("common.signedIn.as", { username: me.user.username })}
      </span>
      <Button variant="link" className="h-auto p-0 text-sm" onClick={() => void signOut()}>
        {t("common.signedIn.logout")}
      </Button>
    </div>
  );
}

// AuthLayout is the centered card used by every screen outside the signed-in
// shell: /setup, /login, the error and not-found pages and, for a signed-in
// user, /welcome. Title above the card, plain-HTTP notice inside it, version
// footer under it (D-38, UI-SPEC "Auth screens"). The two pages a signed-in
// user can reach here pass showSignOut.
export function AuthLayout({
  title,
  subtitle,
  wide = false,
  showSignOut = false,
  children,
}: AuthLayoutProps) {
  const { t } = useTranslation();
  const plainHttp = isPlainHttpRemote();

  return (
    <main className="flex min-h-svh flex-col items-center gap-4 bg-background px-4 pt-16 pb-6 text-foreground">
      <div className={cn("flex w-full flex-col gap-4", wide ? "max-w-120" : "max-w-100")}>
        {showSignOut ? <SignedInBar /> : null}
        <TierPlanLogo
          variant="wordmark"
          decorative={title === t("common.brand.wordmark")}
          className="h-10 self-start"
        />
        <div className="flex flex-col gap-1">
          <h1
            className="line-clamp-2 text-2xl leading-tight font-medium wrap-anywhere"
            title={title}
          >
            {title}
          </h1>
          {subtitle ? <p className="text-sm text-muted-foreground">{subtitle}</p> : null}
        </div>
        {plainHttp || children ? (
          <Card className="py-6">
            <CardContent className="flex flex-col gap-4 px-6">
              {plainHttp ? (
                <Alert variant="warning">
                  <TriangleAlert aria-hidden="true" />
                  <AlertDescription>{t("common.plainHttp.warning")}</AlertDescription>
                </Alert>
              ) : null}
              {children}
            </CardContent>
          </Card>
        ) : null}
      </div>
      <VersionFooter />
    </main>
  );
}
