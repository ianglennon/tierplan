// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Lock } from "lucide-react";
import { useEffect, useRef, useState } from "react";

import {
  type ACMEBlock,
  type ACMERefusal,
  ACMERunError,
  type HTTPSACME,
  type HTTPSStatus,
  httpsStatusPoll,
  isLockProblem,
  useIssueCertificate,
  useRenewNow,
} from "@/api/https";
import { useNow } from "@/components/relative-time";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Spinner } from "@/components/ui/spinner";
import { type CardState, deriveEnvOverlay } from "@/features/https/card-state";
import { ChallengeChangedPrompt } from "@/features/https/challenge-changed-prompt";
import { formatDate } from "@/features/https/copy";
import { useACMERun } from "@/features/https/https-run-stream";
import { LockedNote } from "@/features/https/locked-note";
import { RunPanel } from "@/features/https/run-panel";
import { Rich } from "@/features/https/state-alerts";
import { useT } from "@/i18n/use-t";
import { formatClock } from "@/lib/format";
import { toastSuccess } from "@/lib/toast";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

// The line that says why Issue and Renew cannot be used; the Stop button is described by it too.
export const ISSUE_NOTE_ID = "acme-issue-note";
const NOTE_ID = ISSUE_NOTE_ID;
const REFUSALS_ID = "acme-issue-refusals";
// The two lock lines of Issue certificate and Renew now while TIERPLAN_ACME=off; they replace the
// ordinary reason, so the buttons are described by them (D-04, UI-SPEC A1).
const ISSUE_LOCK_ID = "acme-issue-lock";

type Which = "issue" | "renew";

// Which buttons a state draws (UI-SPEC "State matrix", Buttons column). Issue is for a certificate
// that is missing or no longer covers the names; Renew now for one that is served, or was and expired.
function buttonsFor(
  state: CardState,
  served: boolean,
  mismatch: boolean,
): { issue: boolean; renew: boolean } {
  switch (state) {
    case "B":
    case "L":
      return { issue: true, renew: false };
    case "C":
    case "H":
      return { issue: true, renew: true };
    case "D":
      return { issue: false, renew: true };
    case "E":
    case "K":
      return { issue: true, renew: true };
    case "F":
    case "G":
      return { issue: !served || mismatch, renew: served };
    default:
      // A has no account, I and J are paused by certificate files: nothing to press.
      return { issue: false, renew: false };
  }
}

interface IssueSectionProps {
  status: HTTPSStatus;
  acme: HTTPSACME;
  state: CardState;
  canWrite: boolean;
  // Told whether the run this page followed has just ended in failure and the status has been read
  // again, so the card can announce the error panel once (role alert) instead of on every reload.
  onRunFailed?: (failed: boolean) => void;
  // Told whether a run is active as this page sees it: the status's run, or one the page has started or
  // is following before the status lists it. The Stop button is disabled by it.
  onRunActive?: (active: boolean) => void;
  // True while the Validation section holds a draft that is not saved (UI-SPEC J4): an order would use
  // the saved settings, not the ones the page shows, so the buttons wait for the save.
  validationUnsaved?: boolean;
}

// IssueSection is the "Run and actions" part of the Let's Encrypt card (ACME-04, ACME-06, ACME-07,
// ACME-10, D-09): the list of what stops an order, the Issue certificate and Renew now buttons with
// their waits, and the live panel of the run. A run started elsewhere (another tab, the scheduler, a
// resume after a restart) is picked up from the status and followed the same way.
export function IssueSection({
  status,
  acme,
  state,
  canWrite,
  onRunFailed,
  onRunActive,
  validationUnsaved = false,
}: IssueSectionProps) {
  const t = useT();
  const queryClient = useQueryClient();
  const now = useNow();
  const issue = useIssueCertificate();
  const renew = useRenewNow();

  const [jobId, setJobId] = useState<string | null>(null);
  // The button this page pressed, and the run it started once the server has answered. The spinner
  // stays on that button for that run only: a later run (the scheduler, another tab) gets none.
  const [startedBy, setStartedBy] = useState<{ which: Which; jobId: string | null } | null>(null);
  // What the server refused when a button was used, shown like the status's own list (the SPA never
  // makes a refusal up). It belongs to the names it was refused for.
  const [refused, setRefused] = useState<{ refusals: ACMERefusal[]; names: string } | null>(null);
  const [told, setTold] = useState<ACMEBlock | null>(null);
  const [reused, setReused] = useState<string | null>(null);
  const [failed, setFailed] = useState(false);
  const [refreshed, setRefreshed] = useState<string | null>(null);
  const handled = useRef<string | null>(null);
  // Runs whose stream the server said is gone: they are not attached again from a stale status.
  const abandoned = useRef(new Set<string>());

  const stream = useACMERun(jobId);
  const current = jobId !== null && stream.run?.jobId === jobId;

  // Attach to a run the status lists that this page did not start.
  const activeJobId = acme.activeRun?.jobId;
  useEffect(() => {
    if (activeJobId && activeJobId !== jobId && !abandoned.current.has(activeJobId)) {
      setJobId(activeJobId);
    }
  }, [activeJobId, jobId]);

  // While the stream is down the status is read every 5 seconds, so the end of the run still shows.
  const down = jobId !== null && current && !stream.finished && stream.status === "reconnecting";
  useQuery(httpsStatusPoll(down));

  // The run ended: the status card learns of the new certificate, and a success is told.
  const runState = current ? stream.run?.state : undefined;
  const runKind = current ? stream.run?.kind : undefined;
  useEffect(() => {
    if (!jobId || !current || !stream.finished || handled.current === jobId) return;
    handled.current = jobId;
    void queryClient.invalidateQueries({ queryKey: ["https", "status"] }).then(() => {
      setRefreshed(jobId);
    });
    if (runState === "done") {
      toastSuccess(t(runKind === "renew" ? "https.issue.renewed" : "https.issue.issued"));
    }
  }, [jobId, current, stream.finished, runState, runKind, queryClient, t]);

  // A run of this session ended in failure and the status shows its error: the panel is fresh.
  const failedNow = current && stream.finished && runState === "failed" && refreshed === jobId;
  useEffect(() => {
    onRunFailed?.(failedNow);
  }, [failedNow, onRunFailed]);

  // Let's Encrypt was stopped: the last run's panel and notes speak of a certificate that is no longer
  // served (UI-SPEC state L: the status card is back on Self-signed), so they are put away. The section
  // is not keyed on the mode, since a first issue turns it on and its "Certificate issued." stays.
  const prevMode = useRef(acme.mode);
  useEffect(() => {
    if (prevMode.current === "active" && acme.mode === "off") {
      if (jobId) abandoned.current.add(jobId);
      setJobId(null);
      setStartedBy(null);
      setReused(null);
      setFailed(false);
      setRefreshed(null);
    }
    prevMode.current = acme.mode;
  }, [acme.mode, jobId]);

  // The server no longer has the run, or this person may not read it: nothing more can be followed.
  // The status says how it ended.
  const lost = jobId !== null && (stream.gone || stream.accessLost);
  useEffect(() => {
    if (!lost || !jobId) return;
    abandoned.current.add(jobId);
    setJobId(null);
    void queryClient.invalidateQueries({ queryKey: ["https", "status"] });
  }, [lost, jobId, queryClient]);

  const names = acme.names ?? [];
  const namesKey = names.map((n) => n.name).join(",");
  const orderable = names.filter((n) => !n.refused).map((n) => n.name);
  const namesText = (orderable.length > 0 ? orderable : names.map((n) => n.name)).join(", ");
  const hostText = acme.publicHost !== "" ? acme.publicHost : namesText;

  const served = status.certificate.source === "lets_encrypt";
  const buttons = buttonsFor(state, served, acme.namesMismatch);
  const showButtons = canWrite && (buttons.issue || buttons.renew);

  const statusRefusals = acme.refusals ?? [];
  const serverRefusals = refused !== null && refused.names === namesKey ? refused.refusals : [];
  const refusals = statusRefusals.length > 0 ? statusRefusals : serverRefusals;
  const freshRefusals = statusRefusals.length === 0 && serverRefusals.length > 0;

  const blockedUntil = told !== null && Date.parse(told.until) > now ? told : null;
  const block = acme.blocked ?? blockedUntil;

  const pending = issue.isPending || renew.isPending;
  const streamLive = jobId !== null && !(current && stream.finished);
  const runActive = state === "C" || streamLive;
  useEffect(() => {
    onRunActive?.(runActive);
  }, [runActive, onRunActive]);

  // Why the buttons cannot be used, in the order the card's rules give them. Each reason is a line
  // the buttons are described by, so it is read when they are focused.
  let reason: { key: string; time?: string } | null = null;
  if (runActive) reason = { key: "https.issue.runActive" };
  else if (state === "H") reason = { key: "https.issue.paused" };
  else if (block)
    reason = {
      key: block.reason === "failure_guard" ? "https.issue.failureGuard" : "https.issue.toldToWait",
      time: formatClock(block.until) ?? "",
    };
  // Lowest priority: a run, a pause or a wait is explained first (J4).
  else if (validationUnsaved) reason = { key: "https.issue.validationUnsaved" };

  // TIERPLAN_ACME=off: nothing may order, so Issue and Renew now are locked and the lock is the reason,
  // ahead of every other one because nothing else matters until it is lifted (D-04, UI-SPEC A1).
  const orderLock = acme.locks.acme?.value === "off" ? acme.locks.acme : undefined;
  const locked = orderLock !== undefined;
  if (locked) reason = null;

  // TIERPLAN_ACME=on: TierPlan orders again by itself once the refusals are fixed (D-07).
  const envOn = deriveEnvOverlay(status).env === "on";
  const issueBlockedByNames = refusals.length > 0;
  const disabledAll = pending || reason !== null || locked;
  const issueDisabled = disabledAll || issueBlockedByNames;
  const issueDescribedBy = locked
    ? ISSUE_LOCK_ID
    : reason
      ? NOTE_ID
      : issueBlockedByNames
        ? REFUSALS_ID
        : undefined;
  const renewDescribedBy = locked ? ISSUE_LOCK_ID : reason ? NOTE_ID : undefined;

  const press = (which: Which) => {
    if (which === "issue" ? issueDisabled : disabledAll) return;
    setFailed(false);
    setReused(null);
    setRefused(null);
    setTold(null);
    setStartedBy({ which, jobId: null });
    (which === "issue" ? issue : renew).mutate(undefined, {
      onSuccess: (started) => {
        if (started.reused) {
          setStartedBy(null);
          setReused(started.validUntil ?? "");
        } else if (started.jobId) {
          abandoned.current.delete(started.jobId);
          setStartedBy({ which, jobId: started.jobId });
          setJobId(started.jobId);
        }
      },
      onError: (error) => {
        setStartedBy(null);
        // TIERPLAN_ACME now fixes the switch: the status is read again (by the mutation) and the
        // buttons redraw locked. Nothing is told as a failure.
        if (isLockProblem(error)) return;
        if (!(error instanceof ACMERunError)) {
          setFailed(true);
          return;
        }
        switch (error.type) {
          case "acme_refused":
            if (error.refusals.length > 0)
              setRefused({ refusals: error.refusals, names: namesKey });
            else setFailed(true);
            return;
          case "acme_blocked":
            if (error.blocked) setTold(error.blocked);
            else setFailed(true);
            return;
          case "acme_run_active":
          case "acme_no_account":
          case "acme_paused":
          case "acme_cert_files":
          case "acme_not_active":
            // The page was out of date: the status says what the card is now.
            void queryClient.invalidateQueries({ queryKey: ["https", "status"] });
            return;
          default:
            setFailed(true);
        }
      },
    });
  };

  if (state === "I" || state === "J") return null;

  // Busy while the press is in flight, then while the run it started is the one followed and active.
  const ownRun = startedBy?.jobId != null && startedBy.jobId === jobId && runActive;
  const issueBusy = startedBy?.which === "issue" && (pending || ownRun);
  const renewBusy = startedBy?.which === "renew" && (pending || ownRun);
  // Only the button that can be used is the card's one filled button (UI-SPEC one-primary rule).
  const issueVariant = issueDisabled ? "outline" : "default";

  return (
    <div className="flex flex-col gap-4">
      {refusals.length > 0 ? (
        <Alert variant="destructive" role={freshRefusals ? "alert" : "note"} id={REFUSALS_ID}>
          <AlertTitle>{t("https.refusal.title")}</AlertTitle>
          <AlertDescription>
            <ul className="flex flex-col gap-3">
              {refusals.map((r) => (
                <RefusalItem key={`${r.code}:${r.host ?? ""}:${r.path ?? ""}`} refusal={r} />
              ))}
            </ul>
            {envOn ? (
              <p className="mt-3 text-xs text-muted-foreground">
                <Rich text={t("https.envState.retryRefusals")} />
              </p>
            ) : null}
          </AlertDescription>
        </Alert>
      ) : null}

      {showButtons ? (
        <>
          {/* Saved validation settings no order has used yet (D-03). Not while a run is active: the run
              panel takes over. It carries no button, since the buttons are the next elements below. */}
          {acme.validationUntested && !runActive ? (
            <ChallengeChangedPrompt
              method={acme.challengeType}
              variant={buttons.renew ? "renew" : "issue"}
            />
          ) : null}
          <p className="text-sm text-muted-foreground">{t("https.issue.helper")}</p>
          {orderLock ? (
            <LockedNote id={ISSUE_LOCK_ID} lock={orderLock} variant="order" />
          ) : reason ? (
            <p id={NOTE_ID} className="text-sm text-muted-foreground">
              {t(reason.key, { time: reason.time })}
            </p>
          ) : null}
          <div className="flex flex-wrap gap-2">
            {buttons.issue ? (
              <Button
                type="button"
                variant={issueVariant}
                aria-disabled={issueDisabled}
                aria-describedby={issueDescribedBy}
                className={`${TOUCH_HEIGHT} max-md:w-full aria-disabled:cursor-not-allowed aria-disabled:opacity-50`}
                onClick={() => press("issue")}
              >
                {issueBusy ? (
                  <>
                    <Spinner aria-hidden="true" role="presentation" />
                    {t("https.issue.issuing")}
                  </>
                ) : (
                  <>
                    {locked ? <Lock aria-hidden="true" /> : null}
                    {t("https.issue.issue")}
                  </>
                )}
              </Button>
            ) : null}
            {buttons.renew ? (
              <Button
                type="button"
                variant="outline"
                aria-disabled={disabledAll}
                aria-describedby={renewDescribedBy}
                className={`${TOUCH_HEIGHT} max-md:w-full aria-disabled:cursor-not-allowed aria-disabled:opacity-50`}
                onClick={() => press("renew")}
              >
                {renewBusy ? (
                  <>
                    <Spinner aria-hidden="true" role="presentation" />
                    {t("https.issue.renewing")}
                  </>
                ) : (
                  <>
                    {locked ? <Lock aria-hidden="true" /> : null}
                    {t("https.issue.renew")}
                  </>
                )}
              </Button>
            ) : null}
          </div>
        </>
      ) : null}

      {/* Without settings.write there are no buttons, but the lock still explains why nothing orders. */}
      {orderLock && !canWrite && (buttons.issue || buttons.renew) ? (
        <LockedNote id={ISSUE_LOCK_ID} lock={orderLock} variant="order" />
      ) : null}

      {state === "A" && canWrite ? (
        <p className="text-sm text-muted-foreground">{t("https.issue.noAccount")}</p>
      ) : null}

      {reused !== null ? (
        <Alert role="status">
          <AlertDescription className="text-foreground">
            {t("https.issue.reused", { date: formatDate(reused) })}
          </AlertDescription>
        </Alert>
      ) : null}

      {failed ? (
        <Alert variant="destructive">
          <AlertDescription>{t("common.mutationFailed")}</AlertDescription>
        </Alert>
      ) : null}

      {current ? (
        <RunPanel
          stream={stream}
          names={namesText}
          host={hostText}
          validUntil={refreshed === jobId && served ? acme.certificate?.notAfter : undefined}
        />
      ) : null}
    </div>
  );
}

// RefusalItem is one reason an order cannot start: the reason in 600 weight, the fix under it. The
// host and path are the server's, drawn as text and broken anywhere so a long name cannot widen the
// page.
function RefusalItem({ refusal }: { refusal: ACMERefusal }) {
  const t = useT();
  const host = refusal.host ? refusal.host : t("https.acme.publicHostFallback");
  const hosts = refusal.hosts ?? [];
  const values =
    refusal.code === "agent_dial_host"
      ? { count: refusal.count ?? hosts.length, hosts: hosts.join(", ") }
      : { host, path: refusal.path ?? "" };
  return (
    <li className="min-w-0 break-all" data-code={refusal.code}>
      <span className="block font-semibold">
        {t(`https.refusal.${refusal.code}.reason`, values)}
      </span>
      <span className="block font-normal">{t(`https.refusal.${refusal.code}.fix`)}</span>
    </li>
  );
}
