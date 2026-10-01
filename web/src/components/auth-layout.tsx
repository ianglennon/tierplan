// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { TriangleAlert } from "lucide-react";
import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";

import { VersionFooter } from "@/components/version-footer";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Card, CardContent } from "@/components/ui/card";
import { cn } from "@/lib/utils";

// Hosts where plain http: is not a network exposure (D-12).
const LOCAL_HOSTNAMES = new Set(["localhost", "127.0.0.1", "[::1]", "::1"]);

// isPlainHttpRemote is true when the page was loaded over http: from a host
// other than the machine itself, so typed passwords cross the network readable.
export function isPlainHttpRemote(loc: Pick<Location, "protocol" | "hostname"> = window.location): boolean {
  return loc.protocol === "http:" && !LOCAL_HOSTNAMES.has(loc.hostname);
}

interface AuthLayoutProps {
  title: string;
  subtitle?: string;
  wide?: boolean;
  children?: ReactNode;
}

// AuthLayout is the centered card used by every pre-auth screen (/setup and
// /login): title above the card, plain-HTTP notice inside it, version footer
// under it (D-38, UI-SPEC "Auth screens").
export function AuthLayout({ title, subtitle, wide = false, children }: AuthLayoutProps) {
  const { t } = useTranslation();
  const plainHttp = isPlainHttpRemote();

  return (
    <main className="flex min-h-svh flex-col items-center gap-4 bg-background px-4 pt-16 pb-6 text-foreground">
      <div className={cn("flex w-full flex-col gap-4", wide ? "max-w-120" : "max-w-100")}>
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
