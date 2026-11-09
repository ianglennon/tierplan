// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { TierPlanLogo } from "@/components/tierplan-logo";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { VersionFooter } from "@/components/version-footer";
import { useSignOut } from "@/features/account/sign-out";
import { meQueryOptions } from "@/hooks/use-me";
import { cn } from "@/lib/utils";

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
// user, /welcome. Title above the card, version footer under it (D-38, UI-SPEC
// "Auth screens"). The two pages a signed-in user can reach here pass showSignOut.
// The card exists only when there are children: no TierPlan page is served over
// plain HTTP, so there is no insecure-connection notice to put in it (D-01).
export function AuthLayout({
  title,
  subtitle,
  wide = false,
  showSignOut = false,
  children,
}: AuthLayoutProps) {
  const { t } = useTranslation();
  // When the title is just the product name, the wordmark already says it: the logo is the
  // heading and the name is not written out again under it.
  const logoIsTitle = title === t("common.brand.wordmark");
  const logo = <TierPlanLogo variant="wordmark" className="h-10 self-start" />;

  return (
    <main className="flex min-h-svh flex-col items-center gap-4 bg-background px-4 pt-16 pb-6 text-foreground">
      <div className={cn("flex w-full flex-col gap-4", wide ? "max-w-120" : "max-w-100")}>
        {showSignOut ? <SignedInBar /> : null}
        {logoIsTitle ? (
          <div className="flex flex-col gap-1">
            <h1 className="flex">{logo}</h1>
            {subtitle ? <p className="text-sm text-muted-foreground">{subtitle}</p> : null}
          </div>
        ) : (
          <>
            {logo}
            <div className="flex flex-col gap-1">
              <h1
                className="line-clamp-2 text-2xl leading-tight font-medium wrap-anywhere"
                title={title}
              >
                {title}
              </h1>
              {subtitle ? <p className="text-sm text-muted-foreground">{subtitle}</p> : null}
            </div>
          </>
        )}
        {children ? (
          <Card className="py-6">
            <CardContent className="flex flex-col gap-4 px-6">{children}</CardContent>
          </Card>
        ) : null}
      </div>
      <VersionFooter />
    </main>
  );
}
