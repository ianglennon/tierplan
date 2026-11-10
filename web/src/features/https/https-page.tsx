// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";

import { httpsStatusQuery } from "@/api/https";
import { AcmeCard } from "@/features/https/acme-card";
import { CertificateCard } from "@/features/https/certificate-card";
import { TestDirectoryAlert } from "@/features/https/state-alerts";
import { useT } from "@/i18n/use-t";

// HttpsPage is Settings > HTTPS: the heading, a line saying what the page is for, then the
// certificate the listener serves now. It sits in the narrow (896px) Settings column, 32px
// between blocks.
export function HttpsPage() {
  const t = useT();
  const acme = useQuery(httpsStatusQuery).data?.acme;
  return (
    <div className="flex flex-col gap-8">
      <div className="flex flex-col gap-2">
        <h2 className="text-lg font-semibold">{t("https.title")}</h2>
        <p className="text-sm text-muted-foreground">{t("https.description")}</p>
      </div>
      {acme?.directoryOverridden ? <TestDirectoryAlert url={acme.directoryUrl} /> : null}
      <CertificateCard />
      <AcmeCard />
    </div>
  );
}
