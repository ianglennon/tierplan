// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import { Lock } from "lucide-react";
import { useState } from "react";

import { type HTTPSStatus, httpsStatusQuery } from "@/api/https";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Separator } from "@/components/ui/separator";
import { Skeleton } from "@/components/ui/skeleton";
import { SettingsCard } from "@/features/environments/settings-card";
import { AccountSection } from "@/features/https/account-section";
import {
  accountMode,
  advancedMode,
  deriveCardState,
  deriveEnvOverlay,
  validationMode,
} from "@/features/https/card-state";
import { errorValues } from "@/features/https/copy";
import { ErrorPanel } from "@/features/https/error-panel";
import { ISSUE_NOTE_ID, IssueSection } from "@/features/https/issue-section";
import { LeftOverRecords } from "@/features/https/leftover-records";
import { LockedNote, type LockInfo } from "@/features/https/locked-note";
import { NamesSection } from "@/features/https/names-section";
import { ProfileSection } from "@/features/https/profile-section";
import { StateAlerts } from "@/features/https/state-alerts";
import { StopDialog } from "@/features/https/stop-dialog";
import { ValidationSection } from "@/features/https/validation-section";
import { usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

// The two lock lines of Stop using Let's Encrypt while TIERPLAN_ACME=on.
const STOP_LOCK_ID = "acme-stop-lock";

// AcmeCard is the "Let's Encrypt" card of Settings > HTTPS. It is built in sections in the order of
// the UI-SPEC: the validation method with its detail, the names, the account, then the slots later plans
// fill (the Advanced profile, issue and renew with the run panel, the alerts and the stop row).
// Every part follows deriveCardState, so no section works out a state of its own.
export function AcmeCard() {
  const t = useT();
  const status = useQuery(httpsStatusQuery);
  const canWrite = usePermission("settings.write");

  return (
    <SettingsCard id="lets-encrypt" title={t("https.acme.title")}>
      {status.isPending ? (
        <div className="flex flex-col gap-4" data-testid="acme-loading">
          <Skeleton className="h-5 w-64" />
          <Skeleton className="h-5 w-full" />
          <Skeleton className="h-5 w-48" />
        </div>
      ) : null}
      {status.isError ? (
        <Alert variant="destructive">
          <AlertDescription>{t("common.errors.unreachable")}</AlertDescription>
        </Alert>
      ) : null}
      {status.data?.acme ? <AcmeBody status={status.data} canWrite={canWrite} /> : null}
    </SettingsCard>
  );
}

function AcmeBody({ status, canWrite }: { status: HTTPSStatus; canWrite: boolean }) {
  const t = useT();
  const acme = status.acme;
  // True right after a run of this session failed; a failure read from the ledger is only a note.
  const [freshFailure, setFreshFailure] = useState(false);
  // True while a run is active as the issue section sees it, which is sooner than the status says C.
  const [runActive, setRunActive] = useState(false);
  // True while the validation draft differs from what is saved: an order would not use what the page shows.
  const [validationUnsaved, setValidationUnsaved] = useState(false);
  if (!acme) return null;
  const state = deriveCardState(status);
  // A restore pause with no account yet keeps the registration form, but the server refuses it.
  const paused = acme.pausedReason !== "";
  const accountView = accountMode(state, canWrite);
  const mode = accountView === "form" && paused ? "form-disabled" : accountView;
  const advanced = advancedMode(state, canWrite);
  // The directory is read from the CA, which the server refuses while paused or with certificate files.
  const listable = acme.pausedReason === "" && !status.certFiles;
  // TIERPLAN_ACME=on keeps Let's Encrypt on, so Stop is locked (D-04); Issue certificate never is.
  const stopLock = acme.locks.acme?.value === "on" ? acme.locks.acme : undefined;

  return (
    <div className="flex flex-col gap-6" data-state={state}>
      <div className="flex flex-col gap-4">
        <p className="text-sm text-muted-foreground">{t("https.acme.description")}</p>
        <ValidationSection
          acme={acme}
          mode={validationMode(state, canWrite)}
          onDirtyChange={setValidationUnsaved}
        />
      </div>

      <Separator />
      <NamesSection acme={acme} />

      <Separator />
      <AccountSection account={acme.account} mode={mode} locks={acme.locks} />

      {advanced === "hidden" ? null : (
        <>
          <Separator />
          <ProfileSection acme={acme} mode={advanced} listable={listable} />
        </>
      )}

      <Separator />
      {/* Records TierPlan could not delete, whatever the state and the saved method (D-18). */}
      <LeftOverRecords dns={acme.dns} />
      <StateAlerts status={status} canWrite={canWrite} />
      <IssueSection
        status={status}
        acme={acme}
        state={state}
        canWrite={canWrite}
        onRunFailed={setFreshFailure}
        onRunActive={setRunActive}
        validationUnsaved={validationUnsaved}
      />
      {/* Drawn whatever the mode: a failed first issue leaves Let's Encrypt off, and Stop clears the
          error, so an error the status carries is always one to explain. With the mode off nothing
          retries by itself, so no next automatic attempt is promised. */}
      {acme.lastError ? (
        <ErrorPanel
          error={acme.lastError}
          failures={acme.failures}
          values={errorValues(t, acme.lastError, acme)}
          quiet={paused || acme.blocked !== undefined || acme.mode !== "active"}
          fresh={freshFailure}
          keepsTrying={deriveEnvOverlay(status).env === "on" && !paused}
        />
      ) : null}

      {acme.mode === "active" && state !== "I" && state !== "J" ? (
        canWrite ? (
          <StopRow running={state === "C" || runActive} lock={stopLock} />
        ) : stopLock ? (
          // No button without settings.write, but the lock still says why Let's Encrypt stays on.
          <LockedNote id={STOP_LOCK_ID} lock={stopLock} variant="stop" />
        ) : null
      ) : null}

      {canWrite ? null : (
        <p className="text-xs text-muted-foreground">{t("settings.general.readOnly")}</p>
      )}
    </div>
  );
}

// StopRow is the last part of the card (D-14): a destructive button under a Separator that opens the
// confirmation. It is drawn only while Let's Encrypt is in use and the certificate files are not the
// control. While a run is active the button stays focusable but does nothing, and the run line above
// says why.
function StopRow({ running, lock }: { running: boolean; lock: LockInfo | undefined }) {
  const t = useT();
  const [open, setOpen] = useState(false);
  const locked = lock !== undefined;
  const blocked = running || locked;
  return (
    <>
      <Separator />
      <div className="flex flex-col gap-4">
        <div>
          <Button
            type="button"
            variant="destructive"
            aria-disabled={blocked}
            aria-describedby={locked ? STOP_LOCK_ID : running ? ISSUE_NOTE_ID : undefined}
            className="h-11 max-md:w-full md:h-8 aria-disabled:cursor-not-allowed aria-disabled:opacity-50"
            onClick={() => {
              if (!blocked) setOpen(true);
            }}
          >
            {locked ? <Lock aria-hidden="true" /> : null}
            {t("https.stop.action")}
          </Button>
        </div>
        {lock ? <LockedNote id={STOP_LOCK_ID} lock={lock} variant="stop" /> : null}
      </div>
      <StopDialog open={open} onOpenChange={setOpen} />
    </>
  );
}
