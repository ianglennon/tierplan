// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";

import { type HTTPSStatus, httpsStatusQuery } from "@/api/https";
import { CertExpiry } from "@/components/cert-expiry";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import { SettingsCard } from "@/features/environments/settings-card";
import { useT } from "@/i18n/use-t";
import { formatAbsolute } from "@/lib/format";

// CertificateCard is "Certificate in use": where the served certificate comes from, who issued
// it, every name it covers and when it expires, read from GET /api/https/status. The issuer and
// the names come from a certificate someone else made, so they are shown only as text.
export function CertificateCard() {
  const t = useT();
  const status = useQuery(httpsStatusQuery);

  return (
    <SettingsCard title={t("https.certificate.title")}>
      {status.isPending ? (
        <div className="flex flex-col gap-4" data-testid="certificate-loading">
          <Skeleton className="h-5 w-64" />
          <Skeleton className="h-5 w-full" />
          <Skeleton className="h-5 w-48" />
        </div>
      ) : null}
      {status.isError ? (
        <Alert variant="destructive">
          <AlertDescription>{t("common.errors.unreachable")}</AlertDescription>
        </Alert>
      ) : null}
      {status.data ? <CertificateRows status={status.data} /> : null}
    </SettingsCard>
  );
}

function CertificateRows({ status }: { status: HTTPSStatus }) {
  const t = useT();
  const cert = status.certificate;
  const names = cert.names ?? [];
  const note =
    cert.source === "self_signed"
      ? t("https.certificate.note.self_signed")
      : cert.source === "cert_files"
        ? t("https.certificate.note.cert_files")
        : null;

  return (
    <>
      <dl className="grid grid-cols-1 gap-x-6 gap-y-4 md:grid-cols-[160px_1fr]">
        <dt className="text-sm font-medium text-muted-foreground">
          {t("https.certificate.rows.source")}
        </dt>
        <dd className="min-w-0 break-all text-sm">
          <Badge variant="secondary">{t(`https.certificate.source.${cert.source}`)}</Badge>
        </dd>

        <dt className="text-sm font-medium text-muted-foreground">
          {t("https.certificate.rows.issuer")}
        </dt>
        <dd className="min-w-0 break-all font-mono text-sm">
          {cert.issuer === "" ? t("https.certificate.issuerUnknown") : cert.issuer}
        </dd>

        <dt className="text-sm font-medium text-muted-foreground">
          {t("https.certificate.rows.names")}
        </dt>
        <dd className="min-w-0 break-all text-sm">
          <ul className="flex flex-col gap-2">
            {names.map((name) => (
              <li key={name} className="font-mono">
                {name}
              </li>
            ))}
          </ul>
        </dd>

        <dt className="text-sm font-medium text-muted-foreground">
          {t("https.certificate.rows.expires")}
        </dt>
        <dd className="min-w-0 break-all text-sm">
          <span className="inline-flex flex-wrap items-baseline gap-x-2">
            <CertExpiry notAfter={cert.notAfter} />
            <span className="text-xs text-muted-foreground">{formatAbsolute(cert.notAfter)}</span>
          </span>
        </dd>
      </dl>
      {note ? <p className="text-sm text-muted-foreground">{note}</p> : null}
    </>
  );
}
