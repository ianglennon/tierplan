// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// httpsUrl is the address when it is an https: URL, else undefined. A terms address comes from the
// CA, so it is linked only when it is plainly a web address: a javascript: or data: address from a
// hostile directory must never reach an href.
export function httpsUrl(value: string | undefined): string | undefined {
  if (!value) return undefined;
  try {
    return new URL(value).protocol === "https:" ? value : undefined;
  } catch {
    return undefined;
  }
}
