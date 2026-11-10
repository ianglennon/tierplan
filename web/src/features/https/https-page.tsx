// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { AcmeCard } from "@/features/https/acme-card";
import { CertificateCard } from "@/features/https/certificate-card";
import { useT } from "@/i18n/use-t";

// HttpsPage is Settings > HTTPS: the heading, a line saying what the page is for, then the
// certificate the listener serves now. It sits in the narrow (896px) Settings column, 32px
// between blocks.
export function HttpsPage() {
  const t = useT();
  return (
    <div className="flex flex-col gap-8">
      <div className="flex flex-col gap-2">
        <h2 className="text-lg font-semibold">{t("https.title")}</h2>
        <p className="text-sm text-muted-foreground">{t("https.description")}</p>
      </div>
      <CertificateCard />
      <AcmeCard />
    </div>
  );
}
