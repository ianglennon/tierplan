// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import type { HTTPSStatus } from "@/api/https";
import { Badge } from "@/components/ui/badge";
import { deriveMode } from "@/features/https/card-state";
import { Rich } from "@/features/https/state-alerts";
import { useT } from "@/i18n/use-t";

// ModeLine is the value of the Mode row (D-15, SET-01): the Source badge, the validation method for
// Let's Encrypt, the Test CA badge when it applies, and one sentence saying why this mode is in
// force. deriveMode chooses the kind and the reason; this component only draws them.
export function ModeLine({ status }: { status: HTTPSStatus }) {
  const t = useT();
  const mode = deriveMode(status);
  return (
    <div className="flex min-w-0 flex-col gap-2">
      <span className="inline-flex flex-wrap items-center gap-2">
        <Badge variant="secondary">{t(`https.certificate.source.${mode.kind}`)}</Badge>
        {mode.validation ? (
          <Badge variant="outline">
            {t(
              mode.validation === "dns-01"
                ? "https.validation.dns01.label"
                : "https.validation.http01.label",
            )}
          </Badge>
        ) : null}
        {status.certificate.testCA ? (
          <Badge variant="destructive">{t("https.certificate.testCA")}</Badge>
        ) : null}
      </span>
      <p className="break-words text-sm text-muted-foreground">
        <Rich text={t(`https.mode.reason.${mode.reason}`)} />
      </p>
    </div>
  );
}
