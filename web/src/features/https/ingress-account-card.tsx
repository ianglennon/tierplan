// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import { TriangleAlert } from "lucide-react";

import { type IngressAccount, ingressAccountQuery } from "@/api/ingress-account";
import { useNow } from "@/components/relative-time";
import { StatusDot } from "@/components/status-dot";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { SettingsCard } from "@/features/environments/settings-card";
import { useInstanceAdmin } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { formatRelative } from "@/lib/format";

// IngressAccountCard is the "Ingress certificates" card of Settings > HTTPS (Phase 27, D-04, UI-SPEC
// S6b): whether an Admin accepted Let's Encrypt's terms for this install, which no ingress certificate
// is ordered without, and the pause a restore puts on certificate work. The account is the install's, not
// an environment's, and its routes need the global settings.* permissions that only instance admins hold,
// so the card is drawn for an instance admin and for nobody else, and nobody else's page asks for it.
export function IngressAccountCard() {
  const instanceAdmin = useInstanceAdmin();
  if (!instanceAdmin) return null;
  return <IngressAccountSection />;
}

function IngressAccountSection() {
  const t = useT();
  const account = useQuery(ingressAccountQuery);

  return (
    <SettingsCard id="ingress-certificates" title={t("https.ingressAccount.title")}>
      {account.isPending ? (
        <Skeleton className="h-24 w-full" data-testid="ingress-account-loading" />
      ) : null}
      {account.isError ? (
        <Alert variant="destructive">
          <AlertDescription className="flex flex-wrap items-center gap-3">
            <span>{t("https.ingressAccount.loadFailed")}</span>
            <Button
              type="button"
              variant="outline"
              size="sm"
              disabled={account.isFetching}
              onClick={() => void account.refetch()}
            >
              {t("https.ingressAccount.retry")}
            </Button>
          </AlertDescription>
        </Alert>
      ) : null}
      {account.data ? <IngressAccountBody account={account.data} /> : null}
    </SettingsCard>
  );
}

function IngressAccountBody({ account }: { account: IngressAccount }) {
  const t = useT();
  const now = useNow();
  const accepted = account.termsAccepted;
  const when = account.termsAcceptedAt
    ? formatRelative(account.termsAcceptedAt, new Date(now), t)
    : "";
  // The server names a terms address only when the directory advertised one just now.
  const links = (account.directories ?? []).filter((d) => d.termsOfService !== "");
  const reasonKey =
    account.pausedReason === "restore"
      ? "https.ingressAccount.pausedRestore"
      : "https.ingressAccount.paused";

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-col gap-1">
        <StatusDot
          state={accepted ? "online" : "degraded"}
          label={
            accepted
              ? t("https.ingressAccount.accepted", { time: when }).trim()
              : t("https.ingressAccount.notAccepted")
          }
        />
        {accepted ? null : (
          <p className="text-xs text-muted-foreground">
            {t("https.ingressAccount.notAcceptedNote")}
          </p>
        )}
      </div>
      <p className="break-words text-sm">{t("https.ingressAccount.body")}</p>
      {links.length > 0 ? (
        <ul className="flex flex-col gap-1 text-sm">
          {links.map((d) => (
            <li key={d.issuer}>
              <a
                href={d.termsOfService}
                target="_blank"
                rel="noopener noreferrer"
                className="rounded-sm underline underline-offset-3 focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
              >
                {t("https.ingressAccount.termsLink", { issuer: d.issuer })}
              </a>
            </li>
          ))}
        </ul>
      ) : null}
      {account.paused ? (
        <Alert variant="warning">
          <TriangleAlert aria-hidden="true" />
          <AlertDescription>{t(reasonKey, { reason: account.pausedReason })}</AlertDescription>
        </Alert>
      ) : null}
    </div>
  );
}
