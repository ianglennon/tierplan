// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import type { ACMEDirectory, HTTPSACME, HTTPSStatus } from "@/api/https";
import type { ACMELastError } from "@/features/https/copy";
import type { ACMERun, ACMEStep } from "@/features/https/https-run-stream";

// The Let's Encrypt block of the status on a fresh install that has a public address: no account
// yet, one name that can be ordered, nothing refused.
export function httpsACME(over: Partial<HTTPSACME> = {}): HTTPSACME {
  return {
    challengeType: "http-01",
    directoryOverridden: false,
    directoryUrl: "https://acme-v02.api.letsencrypt.org/directory",
    dns: { leftOver: [], provider: "cloudflare", resolvers: [], tokenSaved: false },
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
    validationUntested: false,
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

// One step of a certificate run as the stream sends it.
export function acmeStep(over: Partial<ACMEStep> = {}): ACMEStep {
  return { id: "account", label: "Account", state: "waiting", ...over };
}

const STEP_LABELS: Record<string, string> = {
  account: "Account",
  order: "Order",
  challenge: "Challenge",
  validate: "Validate",
  finalize: "Finalize",
  stored: "Stored",
};

// The six steps with every step waiting, except those named in states.
export function acmeSteps(states: Partial<Record<string, ACMEStep["state"]>> = {}): ACMEStep[] {
  return Object.keys(STEP_LABELS).map((id) =>
    acmeStep({ id, label: STEP_LABELS[id] as string, state: states[id] ?? "waiting" }),
  );
}

// A run as the stream sends it in its snapshot and done frames: an admin's first issue, running.
export function acmeRunFrame(over: Partial<ACMERun> = {}): ACMERun {
  return {
    jobId: "job-1",
    kind: "issue",
    trigger: "admin",
    state: "running",
    startedAt: new Date(Date.now() - 5_000).toISOString(),
    items: acmeSteps({ account: "running" }),
    ...over,
  };
}

// The run in progress as the status lists it.
export function acmeActiveRun(over: Partial<NonNullable<HTTPSACME["activeRun"]>> = {}) {
  return {
    challenge: "http-01" as const,
    jobId: "job-1",
    kind: "issue" as const,
    outcome: "running" as const,
    startedAt: new Date(Date.now() - 5_000).toISOString(),
    trigger: "admin" as const,
    ...over,
  };
}

// The last failure as the status lists it: Let's Encrypt could not reach port 80.
export function acmeLastError(over: Partial<ACMELastError> = {}): ACMELastError {
  return {
    at: new Date(Date.now() - 3_600_000).toISOString(),
    code: "validation_unreachable",
    detail:
      '{"type":"urn:ietf:params:acme:error:connection","detail":"Timeout during connect (likely firewall problem)"}',
    host: "tierplan.example.com",
    ...over,
  };
}

// The status of an install that serves a Let's Encrypt certificate: issued a month ago for the
// public host, renewing two thirds through its life, with nothing failing.
export function httpsLetsEncryptStatus(
  acme: Partial<HTTPSACME> = {},
  top: Partial<HTTPSStatus> = {},
): HTTPSStatus {
  const issuedAt = new Date(Date.now() - 30 * 86_400_000).toISOString();
  const notAfter = new Date(Date.now() + 60.5 * 86_400_000).toISOString();
  const base = httpsStatus();
  return {
    ...base,
    certificate: {
      source: "lets_encrypt",
      issuer: "Let's Encrypt R11",
      names: ["tierplan.example.com"],
      notBefore: issuedAt,
      notAfter,
      testCA: false,
    },
    acme: httpsACME({
      mode: "active",
      hasState: true,
      account: {
        contact: "ops@example.com",
        registeredAt: issuedAt,
        termsOfServiceUrl: "",
      },
      certificate: {
        issuedAt,
        names: ["tierplan.example.com"],
        notAfter,
        notBefore: issuedAt,
        renewed: false,
      },
      renewal: {
        basis: "two_thirds",
        nextAt: new Date(Date.now() + 30 * 86_400_000).toISOString(),
      },
      ...acme,
    }),
    ...top,
  };
}

// The same, served from the test-only directory.
export function httpsTestCAStatus(acme: Partial<HTTPSACME> = {}): HTTPSStatus {
  const status = httpsLetsEncryptStatus({ directoryOverridden: true, ...acme });
  return { ...status, certificate: { ...status.certificate, testCA: true } };
}

// The Let's Encrypt certificate expired because renewal kept failing, so the self-signed one is
// served until a renewal works.
export function httpsFallbackStatus(acme: Partial<HTTPSACME> = {}): HTTPSStatus {
  const lets = httpsLetsEncryptStatus({
    failures: 4,
    fallback: { expiredAt: new Date(Date.now() - 86_400_000).toISOString() },
    lastError: acmeLastError(),
    renewal: undefined,
    ...acme,
  });
  return { ...lets, certificate: httpsStatus().certificate };
}

// An install restored from a backup: Let's Encrypt is served as before, but nothing is asked of the
// CA until an admin resumes (D-20).
export function httpsRestoredStatus(acme: Partial<HTTPSACME> = {}): HTTPSStatus {
  return httpsLetsEncryptStatus({ pausedReason: "restore", renewal: undefined, ...acme });
}

// Certificate files are set and Let's Encrypt was never set up: the card is disabled (D-12).
export function httpsCertFilesStatus(acme: Partial<HTTPSACME> = {}): HTTPSStatus {
  const base = httpsStatus();
  return {
    ...base,
    certFiles: true,
    certificate: { ...base.certificate, source: "cert_files", issuer: "Corporate CA" },
    acme: httpsACME({ pausedReason: "cert_files", ...acme }),
  };
}

// Certificate files are set and the Let's Encrypt certificate, order and account are kept (D-13).
export function httpsCertFilesKeptStatus(acme: Partial<HTTPSACME> = {}): HTTPSStatus {
  const lets = httpsLetsEncryptStatus({ pausedReason: "cert_files", renewal: undefined, ...acme });
  return {
    ...lets,
    certFiles: true,
    certificate: { ...lets.certificate, source: "cert_files", issuer: "Corporate CA" },
  };
}

// The public URL moved to another host after the certificate was issued (D-05): the certificate keeps
// its old names until an admin issues again.
export function httpsNamesChangedStatus(acme: Partial<HTTPSACME> = {}): HTTPSStatus {
  return httpsLetsEncryptStatus({
    namesMismatch: true,
    names: [
      { name: "tierplan.example.org", refused: false },
      { name: "agents.example.org", refused: false },
    ],
    ...acme,
  });
}
