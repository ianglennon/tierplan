// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { keyExists } from "@/i18n/use-t";

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
