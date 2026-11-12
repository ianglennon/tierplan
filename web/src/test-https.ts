// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import type {
  ACMEDirectory,
  ACMENameTest,
  ACMETokenTest,
  ACMEValidationResult,
  HTTPSACME,
  HTTPSStatus,
} from "@/api/https";
import type { ACMELastError } from "@/features/https/copy";
import type { ACMERun, ACMEStep } from "@/features/https/https-run-stream";

// The DNS block of the status: Cloudflare, no token anywhere, no resolvers and nothing left over.
export type LeftOverRecord = NonNullable<HTTPSACME["dns"]["leftOver"]>[number];

export function acmeDNSStatus(over: Partial<HTTPSACME["dns"]> = {}): HTTPSACME["dns"] {
  return {
    leftOver: [],
    provider: "cloudflare",
    resolvers: [],
    tokenSaved: false,
    tokenSource: "none",
    ...over,
  };
}

// A TXT record TierPlan could not delete as the status lists it: the last delete failed because the
// provider was unreachable, and the next try is in ten minutes.
export function leftOverRecord(over: Partial<LeftOverRecord> = {}): LeftOverRecord {
  return {
    code: "dns_unavailable",
    createdAt: new Date(Date.now() - 3_600_000).toISOString(),
    lastTryAt: new Date(Date.now() - 120_000).toISOString(),
    name: "_acme-challenge.tierplan.example.com",
    nextTryAt: new Date(Date.now() + 600_000).toISOString(),
    pausedReason: "",
    recordId: "rec-1",
    retryPaused: false,
    ...over,
  };
}

// The Let's Encrypt block of the status on a fresh install that has a public address: no account
// yet, one name that can be ordered, nothing refused.
export function httpsACME(over: Partial<HTTPSACME> = {}): HTTPSACME {
  return {
    challengeType: "http-01",
    directoryOverridden: false,
    directoryUrl: "https://acme-v02.api.letsencrypt.org/directory",
    dns: acmeDNSStatus(),
    failures: 0,
    hasState: false,
    listenPort: 7480,
    locks: {},
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
      trust: {
        checked: true,
        verified: false,
        reason: "self_signed",
        host: "tierplan.example.com",
      },
    },
    agentTrust: "pin",
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

const DNS01_STEP_LABELS: ReadonlyArray<readonly [string, string]> = [
  ["account", "Account"],
  ["order", "Order"],
  ["challenge", "Challenge"],
  ["propagation", "Propagation"],
  ["validate", "Validation"],
  ["finalize", "Finalize"],
  ["stored", "Stored"],
  ["cleanup", "Clean-up"],
];

// The eight steps of a DNS-01 run in the order the server sends them (internal/acme/dns_steps.go
// stepItemsDNS01), every step waiting except those named in states. over gives a step its values.
export function acmeStepsDNS01(
  states: Partial<Record<string, ACMEStep["state"]>> = {},
  over: Partial<Record<string, Partial<ACMEStep>>> = {},
): ACMEStep[] {
  return DNS01_STEP_LABELS.map(([id, label]) =>
    acmeStep({ id, label, state: states[id] ?? "waiting", ...over[id] }),
  );
}

// A run as the stream sends it in its snapshot and done frames: an admin's first issue, running.
export function acmeRunFrame(over: Partial<ACMERun> = {}): ACMERun {
  return {
    jobId: "job-1",
    kind: "issue",
    trigger: "admin",
    challenge: "http-01",
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

// A DNS-01 failure as the status lists it: the TXT record never showed on the resolvers asked.
export function lastErrorDNS(over: Partial<ACMELastError> = {}): ACMELastError {
  return acmeLastError({
    code: "dns_not_visible",
    detail: undefined,
    resolvers: ["1.1.1.1", "192.168.1.1"],
    zone: "example.com",
    ...over,
  });
}

// One name of a token test that passed all four rows, with the record seen by the resolvers asked.
export function tokenTestName(over: Partial<ACMENameTest> = {}): ACMENameTest {
  const name = over.name ?? "tierplan.example.com";
  return {
    info: { resolvers: ["1.1.1.1"], seconds: 4, state: "seen" },
    name,
    record: `_acme-challenge.${name}`,
    steps: [
      { id: "zone", state: "done" },
      { id: "created", state: "done" },
      { id: "read", state: "done" },
      { id: "deleted", state: "done" },
    ],
    zone: "example.com",
    ...over,
  };
}

// A token test as the server answers it: one name, every row done.
export function tokenTestResult(over: Partial<ACMETokenTest> = {}): ACMETokenTest {
  return { names: [tokenTestName()], passed: true, ...over };
}

// The answer of a validation save that stored a DNS-01 token after its test passed.
export function validationResult(over: Partial<ACMEValidationResult> = {}): ACMEValidationResult {
  return {
    challengeType: "dns-01",
    resolvers: [],
    saved: true,
    test: tokenTestResult(),
    tokenSaved: true,
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
      trust: { checked: true, verified: true, host: "tierplan.example.com" },
    },
    agentTrust: "public",
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

// The paths of the certificate files and the verdict on them, as the status carries them while the
// files fix the certificate: a certificate that verifies for the public host.
export function certFilesInfo(
  over: Partial<NonNullable<HTTPSStatus["certFilesInfo"]>> = {},
): NonNullable<HTTPSStatus["certFilesInfo"]> {
  return {
    certPath: "/run/secrets/tls.crt",
    keyPath: "/run/secrets/tls.key",
    trust: { checked: true, verified: true, host: "tierplan.example.com" },
    ...over,
  };
}

// Certificate files are set and Let's Encrypt was never set up: the card is disabled (D-12).
export function httpsCertFilesStatus(acme: Partial<HTTPSACME> = {}): HTTPSStatus {
  const base = httpsStatus();
  return {
    ...base,
    certFiles: true,
    certFilesInfo: certFilesInfo(),
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
    certFilesInfo: certFilesInfo(),
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

// Let's Encrypt is set up (an account, mode active) and nothing has been issued yet: the self-signed
// certificate is served.
export function httpsPendingStatus(acme: Partial<HTTPSACME> = {}): HTTPSStatus {
  const lets = httpsLetsEncryptStatus({ certificate: undefined, renewal: undefined, ...acme });
  return { ...lets, certificate: httpsStatus().certificate, agentTrust: "pin" };
}

// Let's Encrypt was stopped: the self-signed certificate is served and the account and the old
// certificate are kept.
export function httpsStoppedStatus(acme: Partial<HTTPSACME> = {}): HTTPSStatus {
  const lets = httpsLetsEncryptStatus({ mode: "off", renewal: undefined, ...acme });
  return { ...lets, certificate: httpsStatus().certificate, agentTrust: "pin" };
}

// The Let's Encrypt card with locks from the environment (SET-02, D-17): the registered account is
// there (so Advanced and the Issue certificate button show, state B), and the locks are the ones given.
// A lock is { var, stored? }; the variable's name is the one the test wants the page to print.
export function httpsLockedStatus(
  locks: HTTPSACME["locks"],
  acme: Partial<HTTPSACME> = {},
  top: Partial<HTTPSStatus> = {},
): HTTPSStatus {
  return httpsStatus({
    acme: httpsACME({
      account: {
        contact: "ops@example.com",
        registeredAt: "2026-11-10T08:00:00Z",
        termsOfServiceUrl: "",
      },
      locks,
      ...acme,
    }),
    ...top,
  });
}

// The TIERPLAN_ACME lock as the status carries it: the variable's value and what is stored.
export function acmeSwitchLock(
  value: "on" | "off",
  stored: "on" | "off" = "off",
): HTTPSACME["locks"] {
  return { acme: { var: "TIERPLAN_ACME", value, stored } };
}

// An install whose Let's Encrypt is turned on by TIERPLAN_ACME and has no certificate yet: the account
// is registered, nothing is running, refused or failed, and the self-signed certificate is served.
export function httpsEnvOnStatus(
  acme: Partial<HTTPSACME> = {},
  top: Partial<HTTPSStatus> = {},
): HTTPSStatus {
  return { ...httpsPendingStatus({ locks: acmeSwitchLock("on"), ...acme }), ...top };
}

// The same once the certificate is issued: TIERPLAN_ACME is on and a Let's Encrypt certificate is served.
export function httpsEnvOnIssuedStatus(acme: Partial<HTTPSACME> = {}): HTTPSStatus {
  return httpsLetsEncryptStatus({ locks: acmeSwitchLock("on"), ...acme });
}

// TIERPLAN_ACME=off over a Let's Encrypt certificate, order and account that are kept: the self-signed
// certificate is served and Let's Encrypt makes no requests.
export function httpsEnvOffStatus(acme: Partial<HTTPSACME> = {}): HTTPSStatus {
  return httpsStoppedStatus({ locks: acmeSwitchLock("off", "on"), ...acme });
}

// TIERPLAN_ACME=off on an install that never set Let's Encrypt up: nothing is kept.
export function httpsEnvOffEmptyStatus(acme: Partial<HTTPSACME> = {}): HTTPSStatus {
  return httpsStatus({ acme: httpsACME({ locks: acmeSwitchLock("off"), ...acme }) });
}

// The TIERPLAN_ACME_CHALLENGE lock as the status carries it: the effective method and the saved one.
export function challengeLock(
  value: "http-01" | "dns-01",
  stored?: "http-01" | "dns-01",
): HTTPSACME["locks"] {
  return {
    challengeType: { var: "TIERPLAN_ACME_CHALLENGE", value, ...(stored ? { stored } : {}) },
  };
}

// The DNS provider and its token locked together by a Cloudflare token variable (D-05). The token's
// entry names the variable and carries nothing else: no value, no stored value.
export function dnsEnvLocks(
  tokenVar = "TIERPLAN_CLOUDFLARE_API_TOKEN",
  storedProvider = "cloudflare",
): HTTPSACME["locks"] {
  return {
    dnsProvider: { var: "TIERPLAN_DNS_PROVIDER", value: "cloudflare", stored: storedProvider },
    dnsToken: { var: tokenVar },
  };
}

// The TIERPLAN_DNS_RESOLVERS lock: the effective list and the saved one, comma-joined as host:port.
export function resolversLock(value: string, stored?: string): HTTPSACME["locks"] {
  return {
    resolvers: {
      var: "TIERPLAN_DNS_RESOLVERS",
      value,
      ...(stored === undefined ? {} : { stored }),
    },
  };
}

// The Let's Encrypt card with DNS-01 in force and the environment supplying the token: the registered
// account is there and dns.tokenSource is env, unless the test says otherwise.
export function httpsDnsLockedStatus(
  locks: HTTPSACME["locks"],
  dns: Partial<HTTPSACME["dns"]> = {},
  acme: Partial<HTTPSACME> = {},
): HTTPSStatus {
  return httpsLockedStatus(locks, {
    challengeType: "dns-01",
    dns: acmeDNSStatus({ tokenSource: "env", ...dns }),
    ...acme,
  });
}
