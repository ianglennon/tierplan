// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";

import { httpsStatusQuery } from "@/api/https";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Skeleton } from "@/components/ui/skeleton";
import { proxyPinDocsUrl } from "@/features/environments/install-dialog";
import { SettingsCard } from "@/features/environments/settings-card";
import { useT } from "@/i18n/use-t";

// AgentTrustCard says how an agent installed now trusts this server (D-18, D-19, TLS-08): one
// paragraph, chosen by the server's agentTrust, and no controls. The page does not decide it from the
// certificate itself.
export function AgentTrustCard() {
  const t = useT();
  const status = useQuery(httpsStatusQuery);
  const trust = status.data?.agentTrust;

  return (
    <SettingsCard title={t("https.agents.title")}>
      {status.isPending ? (
        <div className="flex flex-col gap-4" data-testid="agent-trust-loading">
          <Skeleton className="h-5 w-full" />
          <Skeleton className="h-5 w-48" />
        </div>
      ) : null}
      {status.isError ? (
        <Alert variant="destructive">
          <AlertDescription>{t("common.errors.unreachable")}</AlertDescription>
        </Alert>
      ) : null}
      {trust === "unverified" ? (
        <p className="break-words text-sm">
          {t("https.agents.unverified.before")}{" "}
          <a
            href={proxyPinDocsUrl()}
            target="_blank"
            rel="noopener noreferrer"
            className="rounded-sm underline underline-offset-3 focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
          >
            {t("https.agents.unverified.link")}
          </a>
          {t("https.agents.unverified.after")}
        </p>
      ) : null}
      {trust === "pin" || trust === "public" ? (
        <p className="break-words text-sm">{t(`https.agents.${trust}`)}</p>
      ) : null}
    </SettingsCard>
  );
}
