// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";

import { applyRestore, checkRestore, RestoreError, uploadRestore } from "@/api/backup";
import { setCsrfToken, suspendSessionReactions } from "@/api/client";
import { instanceQuery } from "@/api/instance";
import { queryClient } from "@/api/query-client";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Spinner } from "@/components/ui/spinner";
import { BackupSummary } from "@/features/backup/backup-summary";
import { RestoreConfirmDialog } from "@/features/backup/restore-confirm-dialog";
import { RestoreFields } from "@/features/backup/restore-fields";
import { sessionProbes, waitForRestoreOutcome } from "@/features/backup/restore-outcome";
import { RestoringOverlay, useBlockUnload } from "@/features/backup/restoring-overlay";
import { useRestoreCheck } from "@/features/backup/use-restore-check";
import { SettingsCard } from "@/features/environments/settings-card";
import { useT } from "@/i18n/use-t";
import { withBase } from "@/lib/base-path";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

interface RestorePanelProps {
  // Called once Bayplan is back on the backup's state and the session is gone.
  // It is given the function that lets the app react to refusals again, to call
  // when the page showing the result is left.
  onRestored: (release: () => void) => void;
}

// RestorePanel is the Restore card of Settings > Backup (D-30, path 3): choose a
// file and its passphrase, check it, read what is in it, confirm with the
// instance name, and wait for Bayplan to restart into it. The file and the
// passphrase stay in this component's state, are dropped once the restore has
// worked or another file is chosen, and go with the component.
export function RestorePanel({ onRestored }: RestorePanelProps) {
  const t = useT();
  const { data: instance } = useQuery(instanceQuery);
  const form = useRestoreCheck({ upload: uploadRestore, check: checkRestore });
  const [dialogOpen, setDialogOpen] = useState(false);
  const [applying, setApplying] = useState(false);
  const [waiting, setWaiting] = useState(false);
  // What went wrong with the last restore, for the dialog.
  const [applyError, setApplyError] = useState<string | null>(null);
  // Nothing answered after the restart: the card says so and claims no result.
  const [timedOut, setTimedOut] = useState(false);
  // A new dialog for each restore that gets as far as the restart, so the name is
  // typed afresh if the dialog comes back.
  const [round, setRound] = useState(0);
  const inFlight = useRef(false);
  // The restart can take longer than the user stays on this page.
  const mounted = useRef(false);
  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
    };
  }, []);
  useBlockUnload(applying);

  async function restore() {
    const checked = form.checked;
    if (!checked || inFlight.current) return;
    inFlight.current = true;
    setApplying(true);
    setApplyError(null);
    setTimedOut(false);
    // From here on a refused request is expected: the restart ends the session.
    const release = suspendSessionReactions();
    let restarting = true;
    try {
      await applyRestore(checked.uploadId, form.passphrase, instance?.instanceName ?? "");
    } catch (err) {
      // An answer that says no means nothing changed. A dropped connection is
      // the restart itself, and the wait below finds out how it went.
      if (err instanceof RestoreError) {
        restarting = false;
        release();
        setApplyError(
          err.detail
            ? `${t("restore.failed.nothingChanged")} ${err.detail}`
            : t("restore.failed.nothingChanged"),
        );
      }
    }
    setApplying(false);
    if (!restarting) {
      inFlight.current = false;
      return;
    }
    setDialogOpen(false);
    setRound((r) => r + 1);
    setWaiting(true);
    const outcome = await waitForRestoreOutcome(sessionProbes());
    setWaiting(false);
    inFlight.current = false;
    if (outcome === "restored") {
      // The session is gone: nothing it cached is true any more.
      form.reset();
      queryClient.clear();
      setCsrfToken(null);
      if (!mounted.current) {
        // The page that would have shown the result, and released the session
        // reactions when left, is already gone. Release them now, or no refusal
        // would ever move the browser again, and go where the dead session leads.
        release();
        window.location.assign(withBase("/login"));
        return;
      }
      onRestored(release);
      return;
    }
    release();
    if (outcome === "timeout") {
      setDialogOpen(false);
      setTimedOut(true);
      return;
    }
    setApplyError(t("restore.failed.rolledBack"));
    setDialogOpen(true);
  }

  return (
    <SettingsCard title={t("restore.title")}>
      <p className="text-sm">{t("restore.body")}</p>
      {timedOut ? (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{t("restore.failed.timeout")}</AlertDescription>
        </Alert>
      ) : null}
      {form.checked ? (
        <div className="flex flex-col gap-4">
          <BackupSummary summary={form.checked.summary} />
          <div className="flex flex-wrap gap-2">
            <Button
              type="button"
              variant="destructive"
              className={`${TOUCH_HEIGHT} max-md:w-full`}
              onClick={() => {
                setApplyError(null);
                setDialogOpen(true);
              }}
            >
              {t("restore.summary.restore")}
            </Button>
            <Button
              type="button"
              variant="ghost"
              className={`${TOUCH_HEIGHT} max-md:w-full`}
              onClick={form.chooseAnother}
            >
              {t("restore.summary.chooseAnother")}
            </Button>
          </div>
        </div>
      ) : (
        <form
          noValidate
          className="flex flex-col gap-4"
          onSubmit={(e) => {
            e.preventDefault();
            void form.runCheck();
          }}
        >
          {form.error ? (
            <Alert variant="destructive" role="alert">
              <AlertDescription>{t(form.error.key, form.error.values)}</AlertDescription>
            </Alert>
          ) : null}
          <fieldset disabled={form.checking} className="contents">
            <RestoreFields
              idPrefix="restore"
              file={form.file}
              fileKey={form.fileKey}
              onFile={form.setFile}
              passphrase={form.passphrase}
              onPassphrase={form.setPassphrase}
              disabled={form.checking}
            />
            <div>
              <Button
                type="submit"
                disabled={!form.file || form.passphrase === ""}
                className={`${TOUCH_HEIGHT} max-md:w-full`}
              >
                {form.checking ? (
                  <>
                    <Spinner aria-hidden="true" />
                    {t("restore.check.pending")}
                  </>
                ) : (
                  t("restore.check.submit")
                )}
              </Button>
            </div>
          </fieldset>
        </form>
      )}
      <RestoreConfirmDialog
        key={round}
        open={dialogOpen}
        name={instance?.instanceName ?? ""}
        pending={applying}
        error={applyError}
        onCancel={() => setDialogOpen(false)}
        onConfirm={() => void restore()}
      />
      {waiting ? <RestoringOverlay /> : null}
    </SettingsCard>
  );
}
