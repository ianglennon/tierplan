// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

type Translate = (key: string, options?: Record<string, unknown>) => string;

// The failure codes the server names (UpdateInfoBody.reason). Anything else, or a code a newer
// server adds, reads as "other" rather than as the code itself.
const KNOWN: ReadonlySet<string> = new Set([
  "auth",
  "rate_limited",
  "unreachable",
  "not_found",
  "unsupported",
  "other",
]);

// reasonText is the sentence for a failed check (UI-SPEC "Check failure reasons"): TierPlan's
// own words for the typed code, with the registry, repository and tag the check was about. The
// registry's own message is never shown, and never reaches this function. Each sentence ends with
// what to do. The values are plain text; the caller renders the result as a text node.
export function reasonText(
  t: Translate,
  reason: string,
  registry: string,
  repository: string,
  tag: string,
): string {
  const code = KNOWN.has(reason) ? reason : "other";
  return t(`updates.reason.${code}`, {
    registry: registry || t("updates.reason.registryFallback"),
    repository: repository || t("updates.reason.repositoryFallback"),
    tag: tag || t("updates.reason.tagFallback"),
  });
}
