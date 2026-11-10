// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import type { ACMEDirectory, HTTPSACME, HTTPSStatus } from "@/api/https";

// The Let's Encrypt block of the status on a fresh install that has a public address: no account
// yet, one name that can be ordered, nothing refused.
export function httpsACME(over: Partial<HTTPSACME> = {}): HTTPSACME {
  return {
    challengeType: "http-01",
    directoryOverridden: false,
    directoryUrl: "https://acme-v02.api.letsencrypt.org/directory",
    failures: 0,
    hasState: false,
    listenPort: 7480,
    mode: "off",
    names: [{ name: "tierplan.example.com", refused: false }],
    namesMismatch: false,
    pausedReason: "",
    profile: "",
    publicHost: "tierplan.example.com",
    refusals: [],
    ...over,
  };
}

// The HTTPS status as the server answers it on a fresh install: the self-signed certificate it
// made, valid for a year, covering the host and its loopback names, and no Let's Encrypt account.
export function httpsStatus(over: Partial<HTTPSStatus> = {}): HTTPSStatus {
  return {
    certFiles: false,
    certificate: {
      source: "self_signed",
      issuer: "TierPlan",
      names: ["tierplan.example.com", "localhost", "127.0.0.1"],
      notBefore: new Date(Date.now() - 86_400_000).toISOString(),
      notAfter: new Date(Date.now() + 364 * 86_400_000).toISOString(),
      testCA: false,
    },
    acme: httpsACME(),
    ...over,
  };
}

// The CA's directory as the server reads it: the terms address and no profiles.
export function acmeDirectory(over: Partial<ACMEDirectory> = {}): ACMEDirectory {
  return {
    overridden: false,
    profiles: [],
    termsOfServiceUrl: "https://letsencrypt.org/documents/LE-SA-v1.5-February-24-2025.pdf",
    url: "https://acme-v02.api.letsencrypt.org/directory",
    ...over,
  };
}
