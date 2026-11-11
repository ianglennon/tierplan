// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import type { ACMETokenTest } from "@/api/https";

interface TokenTestResultProps {
  result: ACMETokenTest;
  // True when the result came from a save that stored nothing.
  notSaved?: boolean;
  // True when a field the test used has changed since: the result is shown greyed.
  stale?: boolean;
}

// TokenTestResult is the per-name result of a DNS token test.
export function TokenTestResult(_props: TokenTestResultProps) {
  return null;
}
