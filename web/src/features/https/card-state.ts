// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import type { HTTPSStatus } from "@/api/https";

// The states of the Let's Encrypt card (UI-SPEC "State matrix"), A to L. State M, no settings.write,
// is not one of them: it is the canWrite = false overlay on every state.
//   A no account, self-signed served          B account, no certificate yet
//   C a run is active                         D Let's Encrypt served, healthy
//   E served, but the names changed           F last run failed, still serving
//   G told to wait, or three failures         H paused after a restore
//   I certificate files, nothing kept         J certificate files, Let's Encrypt state kept
//   K expired, serving self-signed            L stopped, certificate kept
export type CardState = "A" | "B" | "C" | "D" | "E" | "F" | "G" | "H" | "I" | "J" | "K" | "L";

export type AccountMode = "form" | "form-disabled" | "summary" | "summary-disabled";
export type AdvancedMode = "hidden" | "shown" | "disabled";
export type ValidationMode = "editable" | "disabled";

// deriveCardState is the one place that decides which state the card is in, from the status alone.
// The page reads the answer and never works out an expiry fallback or a mismatch itself. The order is
// the matrix's: certificate files and a restore pause everything, a run in progress comes next, then
// the fallback, then what is served.
export function deriveCardState(status: HTTPSStatus): CardState {
  const acme = status.acme;
  if (!acme) return "A";
  // An account is Let's Encrypt state too: it is kept while the files are set (D-13).
  if (status.certFiles || acme.pausedReason === "cert_files") {
    return acme.hasState || acme.account ? "J" : "I";
  }
  // Nothing to resume without an account: the form shows, and the paused card keeps it disabled.
  if (!acme.account) return "A";
  if (acme.pausedReason === "restore") return "H";
  if (acme.activeRun) return "C";
  if (acme.fallback) return "K";
  const served = status.certificate.source === "lets_encrypt";
  if (served && acme.namesMismatch) return "E";
  if (acme.blocked) return "G";
  if (acme.lastError) return "F";
  if (served) return "D";
  if (acme.certificate) return "L";
  return "B";
}

// accountMode says what the Account section draws: the registration form before an account exists,
// the summary after. Either is disabled when the card is paused for certificate files (I and J) and
// without settings.write.
export function accountMode(state: CardState, canWrite: boolean): AccountMode {
  const registered = state !== "A" && state !== "I";
  const disabled = !canWrite || state === "I" || state === "J";
  if (registered) return disabled ? "summary-disabled" : "summary";
  return disabled ? "form-disabled" : "form";
}

// advancedMode says what the Advanced section does: hidden until an account exists (A and I), and
// drawn but disabled while a run is active (C), the card is paused (H and J) or the user cannot write.
export function advancedMode(state: CardState, canWrite: boolean): AdvancedMode {
  if (state === "A" || state === "I") return "hidden";
  if (!canWrite || state === "C" || state === "H" || state === "J") return "disabled";
  return "shown";
}

// validationMode says whether the Validation section can be changed. It is drawn in every state but
// disabled (inputs disabled, no action button) while a run is active (C, so two writers never touch
// the challenge), after a restore (H), with certificate files (I and J) and without settings.write.
// Before an account exists (A) it is editable: the method can be chosen first.
export function validationMode(state: CardState, canWrite: boolean): ValidationMode {
  if (!canWrite || state === "C" || state === "H" || state === "I" || state === "J") {
    return "disabled";
  }
  return "editable";
}

// ModeReason is why the install is in the mode it is in (UI-SPEC "Mode line"). envPending, envOff and
// issuedEnv are the reasons that come from TIERPLAN_ACME; they are derived from the lock data, not
// from the status alone, so deriveMode never produces them.
export type ModeReason =
  | "default"
  | "pending"
  | "envPending"
  | "stopped"
  | "fallback"
  | "envOff"
  | "files"
  | "filesPaused"
  | "issued"
  | "issuedEnv"
  | "pausedRestore";

export type ModeKind = "self_signed" | "cert_files" | "lets_encrypt";

export interface Mode {
  kind: ModeKind;
  // The validation method, for Let's Encrypt only.
  validation?: "http-01" | "dns-01";
  reason: ModeReason;
}

// deriveMode is the one place that decides which TLS mode the install is in and why (D-15, SET-01).
// The kind is certificate files when the status says they fix the certificate, Let's Encrypt when
// that is what is served, self-signed otherwise. Exactly one reason is chosen, in this order:
// certificate files (filesPaused when Let's Encrypt is paused for them, else files), then a restore
// pause of a served Let's Encrypt certificate (pausedRestore), then an expiry fallback (fallback),
// then a served Let's Encrypt certificate (issued), then for a self-signed certificate: Let's
// Encrypt set up with nothing issued yet (pending), Let's Encrypt stopped with state kept (stopped),
// else default. A value the page does not know falls to default, so a newer server never leaves the
// row without a sentence.
export function deriveMode(status: HTTPSStatus): Mode {
  const acme = status.acme;
  const source = status.certificate.source;
  if (status.certFiles || source === "cert_files") {
    return {
      kind: "cert_files",
      reason: acme?.pausedReason === "cert_files" ? "filesPaused" : "files",
    };
  }
  if (source === "lets_encrypt") {
    const validation = acme?.challengeType === "dns-01" ? "dns-01" : acme ? "http-01" : undefined;
    return {
      kind: "lets_encrypt",
      ...(validation ? { validation } : {}),
      reason: acme?.pausedReason === "restore" ? "pausedRestore" : "issued",
    };
  }
  if (acme?.fallback) return { kind: "self_signed", reason: "fallback" };
  if (acme?.mode === "active" && !acme.certificate)
    return { kind: "self_signed", reason: "pending" };
  if (acme?.mode === "off" && acme.hasState) return { kind: "self_signed", reason: "stopped" };
  return { kind: "self_signed", reason: "default" };
}

// hasEnvLocks says whether any HTTPS field is fixed by a TIERPLAN_* variable, which is when the page
// explains locks at the top (SET-02). The public and agent URL locks live on Settings > General and are
// not part of this status, so they never trigger it.
export function hasEnvLocks(status: HTTPSStatus): boolean {
  const locks = status.acme?.locks;
  if (!locks) return false;
  return Object.values(locks).some((lock) => lock !== undefined);
}
