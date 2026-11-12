// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Check, Lock, Minus, TriangleAlert } from "lucide-react";
import { useId } from "react";

import type { HTTPSStatus } from "@/api/https";
import { rich } from "@/features/https/sentence";
import { Rich } from "@/features/https/state-alerts";
import { trustReasonText } from "@/features/https/trust-copy";
import { useT } from "@/i18n/use-t";

type CertFilesInfo = NonNullable<HTTPSStatus["certFilesInfo"]>;

// CertFilesBlock is the read-only details of certificate-files mode (D-16): where the two files are,
// which variable sets each, and whether the certificate in them verifies for the public host. The
// paths come from the operator's environment and are drawn as text, never as links, and nothing here
// can be edited: the files are changed by changing the variables and restarting.
export function CertFilesBlock({ info, host }: { info: CertFilesInfo; host?: string }) {
  const t = useT();
  const headingId = useId();
  return (
    <section
      aria-labelledby={headingId}
      className="flex min-w-0 flex-col gap-4 rounded-lg border p-4"
    >
      <h4 id={headingId} className="text-sm font-semibold">
        {t("https.certFiles.title")}
      </h4>
      <dl className="grid grid-cols-1 gap-x-6 gap-y-4 md:grid-cols-[160px_1fr]">
        <dt className="text-sm font-medium text-muted-foreground">
          {t("https.certFiles.rows.cert")}
        </dt>
        <dd className="min-w-0 break-all text-sm">
          <PathValue path={info.certPath} variable="TIERPLAN_TLS_CERT_FILE" />
        </dd>

        <dt className="text-sm font-medium text-muted-foreground">
          {t("https.certFiles.rows.key")}
        </dt>
        <dd className="min-w-0 break-all text-sm">
          <PathValue path={info.keyPath} variable="TIERPLAN_TLS_KEY_FILE" />
        </dd>

        <dt className="text-sm font-medium text-muted-foreground">
          {t("https.certFiles.rows.trusted")}
        </dt>
        <dd className="min-w-0 break-words text-sm">
          <TrustedValue trust={info.trust} host={host ?? info.trust.host} />
        </dd>
      </dl>
      <p className="text-xs text-muted-foreground">{t("https.certFiles.foot")}</p>
    </section>
  );
}

// The path in mono, then the sentence that says which variable sets it. There is no stored value
// beneath: these two variables have no stored counterpart.
function PathValue({ path, variable }: { path: string; variable: string }) {
  const t = useT();
  return (
    <>
      <code className="block break-all font-mono text-xs">{path}</code>
      <span className="mt-1 flex items-start gap-2 break-normal text-sm text-muted-foreground">
        <Lock
          role="img"
          aria-label={t("setup.instance.url.lockedIcon")}
          className="mt-0.5 size-4 shrink-0"
        />
        <span className="min-w-0">
          <Rich text={t("https.certFiles.setBy", { var: variable })} />
        </span>
      </span>
    </>
  );
}

function TrustedValue({
  trust,
  host,
}: {
  trust: CertFilesInfo["trust"];
  host: string | undefined;
}) {
  const t = useT();
  if (!trust.checked) {
    return (
      <span className="inline-flex items-center gap-2">
        <Minus className="size-4 shrink-0 text-muted-foreground" aria-hidden="true" />
        <span>{t("https.certFiles.notChecked")}</span>
      </span>
    );
  }
  if (trust.verified) {
    return (
      <div className="flex flex-col gap-1">
        <span className="flex items-start gap-2 font-medium">
          <Check className="mt-0.5 size-4 shrink-0 text-status-online-text" aria-hidden="true" />
          <span className="min-w-0">
            {rich(t, "https.certFiles.verified", {
              host: host && host !== "" ? host : t("https.trust.hostFallback"),
            })}
          </span>
        </span>
        <span className="text-xs text-muted-foreground">{t("https.certFiles.hstsYes")}</span>
      </div>
    );
  }
  const reason = trustReasonText(t, trust.reason, host);
  return (
    <div className="flex flex-col gap-1">
      <span className="flex items-start gap-2 font-medium">
        <TriangleAlert
          className="mt-0.5 size-4 shrink-0 text-status-warning-text"
          aria-hidden="true"
        />
        <span>{t("https.certFiles.unverified")}</span>
      </span>
      <span className="break-normal">{reason.sentence}</span>
      <span className="break-normal text-xs text-muted-foreground">{reason.fix}</span>
      <span className="text-xs text-muted-foreground">{t("https.certFiles.hstsNo")}</span>
    </div>
  );
}
