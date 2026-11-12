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
// issuedEnv are the reasons that come from TIERPLAN_ACME (D-15).
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

// EnvOverlay is what TIERPLAN_ACME adds to a card state (UI-SPEC J5, D-04, D-07): an overlay on the
// letters A to L, never a letter of its own.
export interface EnvOverlay {
  // The variable's value, null when it is not set.
  env: "on" | "off" | null;
  // TIERPLAN_ACME is on and TierPlan has no Let's Encrypt certificate yet, with nothing running,
  // refused, failed or told to wait: it orders by itself and the admin presses nothing.
  waiting: boolean;
  // TIERPLAN_ACME is on, DNS-01 is in force and there is no DNS token anywhere, so nothing can be
  // ordered until one is added (D-07). No attempt is made, so no attempt is charged and no error shows.
  waitingForToken: boolean;
  // TIERPLAN_ACME is off and a Let's Encrypt certificate, order or account is kept (D-02).
  offWithState: boolean;
}

// deriveEnvOverlay is the one place that reads TIERPLAN_ACME and the fields around it into what the
// card says. The inputs are acme.locks.acme (the variable), acme.certificate and the served source
// (a certificate exists), acme.activeRun, acme.lastError, acme.refusals and acme.blocked (something is
// happening or was refused), acme.pausedReason and certFiles (nothing may order), acme.hasState and
// acme.account (state is kept) and the DNS-01 token source. No component works any of it out itself.
export function deriveEnvOverlay(status: HTTPSStatus): EnvOverlay {
  const acme = status.acme;
  const value = acme?.locks?.acme?.value;
  const env = value === "on" || value === "off" ? value : null;
  if (!acme || env === null) {
    return { env: null, waiting: false, waitingForToken: false, offWithState: false };
  }
  const stored = acme.hasState || !!acme.account || !!acme.certificate;
  // DNS-01 with no token anywhere is its own waiting state, which says what is missing.
  const noToken = acme.challengeType === "dns-01" && acme.dns.tokenSource === "none";
  // Nothing to order for: no certificate yet, none served, no files, no pause and no run in progress.
  const idle =
    env === "on" &&
    !acme.certificate &&
    status.certificate.source !== "lets_encrypt" &&
    !status.certFiles &&
    acme.pausedReason === "" &&
    !acme.activeRun;
  const waiting =
    idle && !acme.lastError && (acme.refusals ?? []).length === 0 && !acme.blocked && !noToken;
  return { env, waiting, waitingForToken: idle && noToken, offWithState: env === "off" && stored };
}

// deriveMode is the one place that decides which TLS mode the install is in and why (D-15, SET-01).
// The kind is certificate files when the status says they fix the certificate, Let's Encrypt when
// that is what is served, self-signed otherwise. Exactly one reason is chosen, in this order:
// certificate files (filesPaused when Let's Encrypt is paused for them, else files), then a restore
// pause of a served Let's Encrypt certificate (pausedRestore), then an expiry fallback (fallback),
// then TIERPLAN_ACME=off (envOff), then a served Let's Encrypt certificate (issuedEnv when
// TIERPLAN_ACME is on, else issued), then for a self-signed certificate: TIERPLAN_ACME=on with nothing
// issued yet (envPending), Let's Encrypt set up with nothing issued yet (pending), Let's Encrypt
// stopped with state kept (stopped), else default. A value the page does not know falls to default, so
// a newer server never leaves the row without a sentence.
export function deriveMode(status: HTTPSStatus): Mode {
  const acme = status.acme;
  const source = status.certificate.source;
  const env = deriveEnvOverlay(status).env;
  if (status.certFiles || source === "cert_files") {
    return {
      kind: "cert_files",
      reason: acme?.pausedReason === "cert_files" ? "filesPaused" : "files",
    };
  }
  if (source === "lets_encrypt") {
    const validation = acme?.challengeType === "dns-01" ? "dns-01" : acme ? "http-01" : undefined;
    const reason =
      acme?.pausedReason === "restore" ? "pausedRestore" : env === "on" ? "issuedEnv" : "issued";
    return { kind: "lets_encrypt", ...(validation ? { validation } : {}), reason };
  }
  if (acme?.fallback) return { kind: "self_signed", reason: "fallback" };
  if (env === "off") return { kind: "self_signed", reason: "envOff" };
  if (env === "on" && !acme?.certificate) return { kind: "self_signed", reason: "envPending" };
  if (acme?.mode === "active" && !acme.certificate)
    return { kind: "self_signed", reason: "pending" };
  if (acme?.mode === "off" && acme.hasState) return { kind: "self_signed", reason: "stopped" };
  return { kind: "self_signed", reason: "default" };
}

// hasEnvLocks says whether any HTTPS field is fixed by a TIERPLAN_* variable, which is when the page
// explains locks at the top (SET-02). The public and agent URL locks live on Settings > General and are
// not part of this status, so they never trigger it. The HSTS lock counts (D-10, D-17).
export function hasEnvLocks(status: HTTPSStatus): boolean {
  if (status.hsts?.lock !== undefined) return true;
  const locks = status.acme?.locks;
  if (!locks) return false;
  return Object.values(locks).some((lock) => lock !== undefined);
}

// The HSTS lifetime ramp (D-11): the max-age of each of the four steps, in seconds.
export const HSTS_MAX_AGES = [300, 604_800, 2_592_000, 31_536_000] as const;

// What the HSTS card's Status row says (UI-SPEC "Status values"). not_sent_clearing is not_sent while
// max-age=0 goes out, so a browser that trusts the certificate forgets the one-year policy earlier
// versions sent for certificate files (D-23).
export type HstsStatusKey =
  | "sending"
  | "off_clearing"
  | "off_silent"
  | "not_sent"
  | "not_sent_clearing"
  | "withdrawn";

// done, current and later are the ramp as it stands; unused is a step a short-lived certificate never
// reaches (D-22).
export type HstsStepState = "done" | "current" | "later" | "unused";

export interface HstsStep {
  // 1 to 4: five minutes, a week, a month, a year.
  n: 1 | 2 | 3 | 4;
  maxAge: number;
  state: HstsStepState;
}

// The Next step row: when the longer step comes, that this is the last step, or that the certificate
// stops the ramp at a week.
export type HstsNext = { kind: "at"; at: string } | { kind: "final" } | { kind: "capped" };

export interface HstsView {
  state: HstsStatusKey;
  // The effective setting, which TIERPLAN_HSTS decides when it is set.
  enabled: boolean;
  // The choice kept in the database; the Switch's draft is compared with it.
  storedEnabled: boolean;
  // The lock, when TIERPLAN_HSTS fixes the setting.
  lock: { var: string; stored?: string } | undefined;
  maxAge: number;
  step: number;
  // The Next step row, left out when it has nothing to say.
  next: HstsNext | undefined;
  clearing: boolean;
  // Why HSTS is on but not sent: the trust reason code (test_ca when the certificate comes from a test
  // directory), and the host it was judged for. Both only while the state is not_sent.
  reason: string | undefined;
  host: string | undefined;
  withdrawal: NonNullable<HTTPSStatus["hsts"]>["withdrawal"];
  // The time after which no browser can still hold a policy TierPlan sent, only while that is still in
  // the future: the Stop dialog warns until then (D-26).
  forgetBy: string | undefined;
  // The four lifetime steps, shown while the setting is on (steps is empty when it is off).
  steps: HstsStep[];
  // Every step is Later and the list says the lifetime starts when HSTS is sent (not sent, withdrawn).
  startsLater: boolean;
  // Stop using Let's Encrypt is refused while the effective setting is on (D-26).
  blocksStop: boolean;
}

function hstsStatusKey(hsts: NonNullable<HTTPSStatus["hsts"]>): HstsStatusKey {
  switch (hsts.state) {
    case "sending":
      return "sending";
    case "withdrawn":
      return "withdrawn";
    case "off_clearing":
      return "off_clearing";
    case "off_silent":
      return "off_silent";
    case "not_sent":
      return hsts.clearing ? "not_sent_clearing" : "not_sent";
    default:
      // A state this build does not know: say what is certain from the setting alone.
      return hsts.enabled ? "not_sent" : "off_silent";
  }
}

// hstsView is the one place that reads status.hsts into what the HSTS card and the Stop button say
// (TLS-06, D-10 to D-14, D-22, D-23, D-26): the Status row's state, the ramp with its done, current,
// later and unused steps, the Next step row, the trust reason (test_ca when the certificate is from a
// test directory), and whether Stop is blocked. now is the clock the Stop dialog's warning is judged by.
// It returns undefined for a build without the HSTS policy.
export function hstsView(status: HTTPSStatus, now: number = Date.now()): HstsView | undefined {
  const hsts = status.hsts;
  if (!hsts) return undefined;
  const state = hstsStatusKey(hsts);
  const sending = state === "sending";
  const notSent = state === "not_sent" || state === "not_sent_clearing";

  let next: HstsNext | undefined;
  if (sending) {
    if (hsts.step >= 4) next = { kind: "final" };
    else if (hsts.capped && hsts.step >= 2) next = { kind: "capped" };
    else if (hsts.nextStepAt) next = { kind: "at", at: hsts.nextStepAt };
  }

  const steps: HstsStep[] = hsts.enabled
    ? HSTS_MAX_AGES.map((maxAge, i): HstsStep => {
        const n = (i + 1) as HstsStep["n"];
        let stepState: HstsStepState = "later";
        if (sending) {
          if (hsts.capped && n > 2) stepState = "unused";
          else if (n < hsts.step) stepState = "done";
          else if (n === hsts.step) stepState = "current";
        }
        return { n, maxAge, state: stepState };
      })
    : [];

  const forgetMs = hsts.forgetBy ? Date.parse(hsts.forgetBy) : Number.NaN;
  const forgetBy = Number.isFinite(forgetMs) && forgetMs > now ? hsts.forgetBy : undefined;

  return {
    state,
    enabled: hsts.enabled,
    storedEnabled: hsts.storedEnabled,
    lock: hsts.lock
      ? { var: hsts.lock.var, ...(hsts.lock.stored ? { stored: hsts.lock.stored } : {}) }
      : undefined,
    maxAge: hsts.maxAge,
    step: hsts.step,
    next,
    clearing: hsts.clearing,
    reason: notSent
      ? status.certificate.testCA
        ? "test_ca"
        : hsts.notSentReason || "unknown_authority"
      : undefined,
    host: notSent ? status.certificate.trust.host : undefined,
    withdrawal: hsts.withdrawal,
    forgetBy,
    steps,
    startsLater: hsts.enabled && !sending,
    blocksStop: hsts.enabled,
  };
}
