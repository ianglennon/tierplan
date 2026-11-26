// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import { Info } from "lucide-react";
import { Trans } from "react-i18next";

import type { HealthState } from "@/api/environments";

import { type IngressCert, ingressCertificatesQuery } from "@/api/ingress-certs";
import { CertExpiry } from "@/components/cert-expiry";
import { RelativeTime } from "@/components/relative-time";
import { StatusDot } from "@/components/status-dot";
import { Badge } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { useT } from "@/i18n/use-t";

// certDot is the dot of a certificate's state (UI-SPEC Color, certificate state): a certificate that works is green,
// one that is on its way is the hollow or pulsing dot, one that is mending or waiting is amber, one that is broken is
// red and one that is stopped on purpose or retired is grey. A state this build does not know is grey, never green.
function certDot(state: string): HealthState {
  switch (state) {
    case "valid":
    case "uploaded":
      return "online";
    case "queued":
      return "pending";
    case "ordering":
      return "reconnecting";
    case "dns_proven_on_staging":
    case "renewal_failed":
      return "degraded";
    case "failed":
    case "expired":
      return "offline";
    default:
      return "unknown";
  }
}

// sourceKey is the words of where a certificate comes from and, for an ordered one, which directory it is from.
function sourceKey(c: IngressCert): string {
  if (c.source === "upload") return "ingressPage.certs.source.upload";
  return c.issuer === "staging"
    ? "ingressPage.certs.source.staging"
    : "ingressPage.certs.source.production";
}

// CertificatesTab is the Certificates tab of the Ingress page (UIX-03, UI-SPEC S3): every certificate of the
// environment with where it comes from, where it stands, when it was issued, when it expires and is renewed, and why
// the last attempt failed. A staging certificate says so in words and in a badge, never in colour alone (U-06).
export function CertificatesTab({
  envId,
}: {
  envId: string;
  host?: string;
  onShowAll?: () => void;
}) {
  const t = useT();
  const query = useQuery(ingressCertificatesQuery(envId));

  if (query.isPending) {
    return (
      <div>
        <span role="status" className="sr-only">
          {t("ingressPage.certs.loading")}
        </span>
        <Skeleton className="h-48 w-full" aria-hidden="true" />
      </div>
    );
  }

  const rows = query.data?.items ?? [];

  return (
    <div className="flex flex-col gap-3">
      <Table aria-label={t("ingressPage.certs.label")}>
        <TableHeader>
          <TableRow>
            <TableHead scope="col">{t("ingressPage.certs.columns.hostname")}</TableHead>
            <TableHead scope="col">{t("ingressPage.certs.columns.state")}</TableHead>
            <TableHead scope="col">{t("ingressPage.certs.columns.issued")}</TableHead>
            <TableHead scope="col">{t("ingressPage.certs.columns.expires")}</TableHead>
            <TableHead scope="col">{t("ingressPage.certs.columns.renewal")}</TableHead>
            <TableHead scope="col">{t("ingressPage.certs.columns.error")}</TableHead>
            <TableHead scope="col">{t("ingressPage.certs.columns.actions")}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {rows.map((c) => (
            <CertRow key={c.id} cert={c} />
          ))}
        </TableBody>
      </Table>
    </div>
  );
}

function CertRow({ cert: c }: { cert: IngressCert }) {
  const t = useT();
  return (
    <TableRow>
      <TableCell className="align-top whitespace-normal">
        <div className="flex flex-wrap items-center gap-2">
          <span className="font-mono text-xs break-all">{c.hostname}</span>
          {c.staging ? (
            <Badge variant="outline" className="shrink-0">
              {t("ingressPage.certs.staging")}
            </Badge>
          ) : null}
        </div>
        <p className="text-xs text-muted-foreground">{t(sourceKey(c))}</p>
      </TableCell>
      <TableCell className="align-top whitespace-normal">
        <StatusDot state={certDot(c.state)} label={c.stateText} />
        {c.state === "dns_proven_on_staging" ? (
          <p className="text-xs break-words text-muted-foreground">
            {t("ingressPage.certs.stagingNeverServed")}
          </p>
        ) : c.queue ? (
          <p className="text-xs break-words text-muted-foreground">{c.queue.reason}</p>
        ) : !c.ready && c.expiresAt !== undefined ? (
          <p className="text-xs break-words text-muted-foreground">
            {t("ingressPage.certs.notServed")}
          </p>
        ) : null}
      </TableCell>
      <TableCell className="align-top text-sm whitespace-normal">
        {c.issuedAt ? <RelativeTime value={c.issuedAt} /> : t("ingressPage.certs.none")}
      </TableCell>
      <TableCell className="align-top text-sm whitespace-normal">
        {c.expiresAt ? (
          <CertExpiry notAfter={c.expiresAt} renewalFailing={c.state === "renewal_failed"} />
        ) : (
          t("ingressPage.certs.none")
        )}
      </TableCell>
      <TableCell className="align-top text-sm whitespace-normal">
        {c.renewAt ? (
          <span className="inline-flex flex-wrap items-center gap-x-1">
            <RelativeTime value={c.renewAt} />
            {c.renewBasis ? <RenewBasis basis={c.renewBasis} /> : null}
          </span>
        ) : (
          t("ingressPage.certs.none")
        )}
      </TableCell>
      <TableCell className="align-top whitespace-normal">
        {c.lastError ? (
          <div className="flex flex-col gap-0.5">
            <p className="text-xs break-words">{c.lastError.hint}</p>
            <NextAttempt cert={c} />
          </div>
        ) : (
          <span className="text-sm">{t("ingressPage.certs.none")}</span>
        )}
      </TableCell>
      <TableCell className="align-top" />
    </TableRow>
  );
}

// RenewBasis is the sentence the renewal time is based on, in a tooltip that opens on hover and on keyboard focus.
function RenewBasis({ basis }: { basis: string }) {
  const t = useT();
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <button
          type="button"
          aria-label={t("ingressPage.certs.renewBasis")}
          className="inline-flex rounded-sm text-muted-foreground focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none max-md:size-11 max-md:items-center max-md:justify-center"
        >
          <Info aria-hidden="true" className="size-3.5" />
        </button>
      </TooltipTrigger>
      <TooltipContent>{basis}</TooltipContent>
    </Tooltip>
  );
}

// NextAttempt is when the order is tried again: the time the authority asked TierPlan to wait until when it did, else
// the next attempt TierPlan planned. A certificate with neither says nothing.
function NextAttempt({ cert: c }: { cert: IngressCert }) {
  const waiting = c.retryAfterUntil !== undefined;
  const at = c.retryAfterUntil ?? c.nextAttemptAt;
  if (at === undefined) return null;
  return (
    <p className="text-xs text-muted-foreground">
      <Trans
        i18nKey={waiting ? "ingressPage.certs.waitingUntil" : "ingressPage.certs.nextAttempt"}
        ns="ingressPage"
        components={{ time: <RelativeTime value={at} /> }}
      />
    </p>
  );
}
