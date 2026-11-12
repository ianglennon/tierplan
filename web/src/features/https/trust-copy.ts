// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import type { ReactNode } from "react";

import { rich } from "@/features/https/sentence";
import type { useT } from "@/i18n/use-t";

// TRUST_REASONS maps a reason a certificate is not trusted to the i18n keys of its sentence and its fix
// (UI-SPEC "Shared trust reasons"). The cert-files block, the HSTS card and the install dialog all word
// a reason through this one table, so the wording cannot drift. test_ca is not a code the server sends:
// the page picks it when the certificate has the Test CA flag.
export const TRUST_REASONS = {
  self_signed: { sentence: "https.trust.self_signed.sentence", fix: "https.trust.self_signed.fix" },
  unknown_authority: {
    sentence: "https.trust.unknown_authority.sentence",
    fix: "https.trust.unknown_authority.fix",
  },
  test_ca: { sentence: "https.trust.test_ca.sentence", fix: "https.trust.test_ca.fix" },
  expired: { sentence: "https.trust.expired.sentence", fix: "https.trust.expired.fix" },
  name_mismatch: {
    sentence: "https.trust.name_mismatch.sentence",
    fix: "https.trust.name_mismatch.fix",
  },
  no_public_url: {
    sentence: "https.trust.no_public_url.sentence",
    fix: "https.trust.no_public_url.fix",
  },
} as const;

export type TrustReasonCode = keyof typeof TRUST_REASONS;

// trustReasonKey is the keys for a reason code. A code this build does not know, or none, reads as
// unknown_authority: the certificate does not verify and the cause is not one the page can name.
export function trustReasonKey(code: string | undefined): (typeof TRUST_REASONS)[TrustReasonCode] {
  return code !== undefined && Object.hasOwn(TRUST_REASONS, code)
    ? TRUST_REASONS[code as TrustReasonCode]
    : TRUST_REASONS.unknown_authority;
}

// trustReasonText is the sentence and the fix for a reason, with the host drawn as mono text that
// breaks anywhere. An empty host reads "the public host".
export function trustReasonText(
  t: ReturnType<typeof useT>,
  code: string | undefined,
  host: string | undefined,
): { sentence: ReactNode[]; fix: ReactNode[] } {
  const keys = trustReasonKey(code);
  const values = { host: host && host !== "" ? host : t("https.trust.hostFallback") };
  return { sentence: rich(t, keys.sentence, values), fix: rich(t, keys.fix, values) };
}
