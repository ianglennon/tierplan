// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import type { ACMEDirectory, HTTPSACME, HTTPSStatus } from "@/api/https";
import type { ACMERun, ACMEStep } from "@/features/https/https-run-stream";

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
    jobId: "job-1",
    kind: "issue" as const,
    outcome: "running" as const,
    startedAt: new Date(Date.now() - 5_000).toISOString(),
    trigger: "admin" as const,
    ...over,
  };
}
