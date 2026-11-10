// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import type { HTTPSStatus } from "@/api/https";

// The HTTPS status as the server answers it on a fresh install: the self-signed certificate it
// made, valid for a year, covering the host and its loopback names.
export function httpsStatus(over: Partial<HTTPSStatus> = {}): HTTPSStatus {
  return {
    certFiles: false,
    certificate: {
      source: "self_signed",
      issuer: "TierPlan",
      names: ["tierplan.example.com", "localhost", "127.0.0.1"],
      notBefore: new Date(Date.now() - 86_400_000).toISOString(),
      notAfter: new Date(Date.now() + 364 * 86_400_000).toISOString(),
    },
    ...over,
  };
}
