// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import type { ReactNode } from "react";

import type { HealthState } from "@/api/environments";
import type { IngressCert } from "@/api/ingress-certs";
import { CertExpiry } from "@/components/cert-expiry";
import { RelativeTime } from "@/components/relative-time";
import { StatusDot } from "@/components/status-dot";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import { useT } from "@/i18n/use-t";

// certDot is the dot of a certificate's state (UI-SPEC Color, certificate state): a certificate that works is green,
// one that is on its way is the hollow or pulsing dot, one that is mending or waiting is amber, one that is broken is
// red and one that is stopped on purpose or retired is grey. A state this build does not know is grey, never green.
export function certDot(state: string): HealthState {
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

// CertificateSheet is the full record of one certificate (D-12, UI-SPEC S3): a definition list of everything the
// server knows about it. A field with no value is left out, never drawn as a blank, and the last error closes the list
// with its detail only for someone who manages ingress (A-04). The sheet has no control that writes.
export function CertificateSheet({
  cert: c,
  canManage,
  open,
  onOpenChange,
}: {
  cert: IngressCert;
  canManage: boolean;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const t = useT();
  const row = (key: string, label: string, value: ReactNode) => ({ key, label, value });
  const rows = [
    row(
      "source",
      t("ingressPage.certs.sheet.source"),
      c.source === "upload"
        ? t("ingressPage.certs.source.upload")
        : t("ingressPage.certs.sheet.sourceAcme"),
    ),
    c.issuer
      ? row(
          "issuer",
          t("ingressPage.certs.sheet.issuer"),
          c.issuer === "staging"
            ? t("ingressPage.certs.sheet.issuerStaging")
            : t("ingressPage.certs.sheet.issuerProduction"),
        )
      : null,
    row(
      "state",
      t("ingressPage.certs.sheet.state"),
      <StatusDot state={certDot(c.state)} label={c.stateText} />,
    ),
    row(
      "ready",
      t("ingressPage.certs.sheet.ready"),
      c.ready ? t("ingressPage.certs.sheet.yes") : t("ingressPage.certs.sheet.no"),
    ),
    c.issuedAt
      ? row("issued", t("ingressPage.certs.sheet.issued"), <RelativeTime value={c.issuedAt} />)
      : null,
    c.expiresAt
      ? row(
          "expires",
          t("ingressPage.certs.sheet.expires"),
          <CertExpiry notAfter={c.expiresAt} renewalFailing={c.state === "renewal_failed"} />,
        )
      : null,
    c.renewAt
      ? row(
          "renewal",
          t("ingressPage.certs.sheet.renewal"),
          <>
            <RelativeTime value={c.renewAt} />
            {c.renewBasis ? (
              <span className="block text-xs text-muted-foreground">{c.renewBasis}</span>
            ) : null}
          </>,
        )
      : null,
    c.consecutiveFailures > 0
      ? row("failures", t("ingressPage.certs.sheet.failures"), c.consecutiveFailures)
      : null,
    c.nextAttemptAt
      ? row(
          "next",
          t("ingressPage.certs.sheet.nextAttempt"),
          <RelativeTime value={c.nextAttemptAt} />,
        )
      : null,
    c.retryAfterUntil
      ? row(
          "waiting",
          t("ingressPage.certs.sheet.waitingUntil"),
          <RelativeTime value={c.retryAfterUntil} />,
        )
      : null,
    c.requestedAt
      ? row(
          "requested",
          t("ingressPage.certs.sheet.requested"),
          <>
            <RelativeTime value={c.requestedAt} />
            {canManage && c.requestedBy ? (
              <>
                {" "}
                <span>{t("ingressPage.certs.sheet.requestedBy", { user: c.requestedBy })}</span>
              </>
            ) : null}
          </>,
        )
      : null,
    c.unreferencedSince
      ? row(
          "unreferenced",
          t("ingressPage.certs.sheet.unreferenced"),
          <RelativeTime value={c.unreferencedSince} />,
        )
      : null,
    c.stoppedReason ? row("stopped", t("ingressPage.certs.sheet.stopped"), c.stoppedReason) : null,
    c.retiredAt
      ? row("retired", t("ingressPage.certs.sheet.retired"), <RelativeTime value={c.retiredAt} />)
      : null,
    c.retiredReason
      ? row("retiredReason", t("ingressPage.certs.sheet.retiredReason"), c.retiredReason)
      : null,
    c.lastError
      ? row(
          "lastError",
          t("ingressPage.certs.sheet.lastError"),
          <>
            <span className="break-words">{c.lastError.hint}</span>
            {c.lastError.at ? (
              <span className="block text-xs text-muted-foreground">
                <RelativeTime value={c.lastError.at} />
              </span>
            ) : null}
            {canManage && c.lastError.detail ? (
              <span className="mt-1 block rounded-md bg-muted/50 p-2 font-mono text-xs break-all">
                {c.lastError.detail}
              </span>
            ) : null}
          </>,
        )
      : null,
  ].filter((r) => r !== null);

  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent side="right" className="overflow-y-auto sm:max-w-md">
        <SheetHeader>
          <SheetTitle>{t("ingressPage.certs.sheet.title")}</SheetTitle>
          <SheetDescription className="font-mono text-xs break-all">{c.hostname}</SheetDescription>
        </SheetHeader>
        <dl className="flex flex-col gap-3 px-4 pb-4 text-sm">
          {rows.map((r) => (
            <div key={r.key} className="flex flex-col gap-0.5">
              <dt className="text-xs text-muted-foreground">{r.label}</dt>
              <dd>{r.value}</dd>
            </div>
          ))}
        </dl>
      </SheetContent>
    </Sheet>
  );
}
