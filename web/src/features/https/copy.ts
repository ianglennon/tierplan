// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import type { HTTPSACME } from "@/api/https";
import { keyExists } from "@/i18n/use-t";
import { formatClock } from "@/lib/format";

export const ERROR_CLASSES = [
  "rate_limited",
  "validation_unreachable",
  "validation_dns",
  "caa_forbids",
  "identifier_rejected",
  "contact_invalid",
  "tos_action_required",
  "account_gone",
  "profile_invalid",
  "ca_unavailable",
  "ca_untrusted",
  "internal",
  "dns_token_invalid",
  "dns_token_scope",
  "dns_zone_not_found",
  "dns_delegation_target",
  "dns_not_visible",
  "dns_rate_limited",
  "dns_unavailable",
  "validation_txt",
] as const;

type Translate = (key: string, options?: Record<string, unknown>) => string;

// errorClassOf is the class whose copy to show: a class this build does not know falls back to
// "internal" (UI-SPEC "Last-run error panel").
export function errorClassOf(code: string | undefined): (typeof ERROR_CLASSES)[number] {
  return ERROR_CLASSES.find((c) => c === code) ?? "internal";
}

// errorCopy is the headline and the fix of a classified failure. Values the copy names (host, port,
// time, url, profile) are passed in; a fix that names a time has a variant for when none is known,
// so a sentence never ends in "next at .".
export function errorCopy(
  t: Translate,
  code: string | undefined,
  values: Record<string, string | number | undefined> = {},
): { headline: string; fix: string } {
  const cls = errorClassOf(code);
  const base = `https.error.${cls}`;
  const fixKey =
    values.time === undefined && keyExists(`${base}.fixNoTime`)
      ? `${base}.fixNoTime`
      : `${base}.fix`;
  return { headline: t(`${base}.headline`, values), fix: t(fixKey, values) };
}

// formatDate is the day of an RFC 3339 time in the browser's own locale ("10 Nov 2026"); an unreadable
// time is an empty string.
export function formatDate(iso: string | undefined): string {
  if (!iso) return "";
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return "";
  return new Intl.DateTimeFormat(undefined, {
    day: "numeric",
    month: "short",
    year: "numeric",
  }).format(date);
}

export type ACMELastError = NonNullable<HTTPSACME["lastError"]>;

// errorValues are the values a failure's copy names: the host it was about (the public host when the
// CA named none, "the public host" when there is none), the port TierPlan listens on, the profile,
// the terms address, the DNS zone, the delegation target, the resolvers that were asked (comma-joined,
// "the zone's nameservers" when none were named) and the time of the next automatic attempt. The time
// is left out when the status names none, so errorCopy picks the sentence without one.
export function errorValues(
  t: Translate,
  error: ACMELastError,
  acme: HTTPSACME,
): Record<string, string | number | undefined> {
  const host = error.host || acme.publicHost || t("https.acme.publicHostFallback");
  return {
    host,
    port: acme.listenPort,
    profile: error.profile ?? "",
    url: error.termsUrl ?? "",
    zone: error.zone ?? "",
    target: error.target ?? "",
    resolvers: error.resolvers?.length
      ? error.resolvers.join(", ")
      : t("https.error.resolversDefault"),
    time: formatClock(acme.nextAttemptAt),
  };
}
