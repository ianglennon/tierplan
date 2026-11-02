// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { useRef, useState } from "react";

import {
  applySetupRestore,
  checkSetupRestore,
  discardSetupRestoreUpload,
  RestoreError,
  uploadSetupRestore,
} from "@/api/backup";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Spinner } from "@/components/ui/spinner";
import { BackupSummary } from "@/features/backup/backup-summary";
import { RestoreFields } from "@/features/backup/restore-fields";
import { setupProbes, waitForRestoreOutcome } from "@/features/backup/restore-outcome";
import { RestoringOverlay, useBlockUnload } from "@/features/backup/restoring-overlay";
import { useRestoreCheck } from "@/features/backup/use-restore-check";
import { useT } from "@/i18n/use-t";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const PRIMARY = "h-11 w-full md:h-8";

const isSetupComplete = (err: unknown) =>
  err instanceof RestoreError && err.type === "setup_complete";

interface SetupRestoreProps {
  // The setup token the server accepted on step 1. It stays in the wizard's state
  // and goes to the server only as a header on the upload and in the body of the
  // check and the restore.
  token: string;
  onBack: () => void;
}

// SetupRestore is the wizard's restore branch (D-30, path 1): the same check as
// Settings, then one press of "Restore backup" with no name to type, because a
// fresh install has nothing to lose. Once TierPlan is back it goes to sign-in with
// a notice. A 409 setup_complete means someone finished setup meanwhile.
export function SetupRestore({ token, onBack }: SetupRestoreProps) {
  const t = useT();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [applying, setApplying] = useState(false);
  const [waiting, setWaiting] = useState(false);
  const [failure, setFailure] = useState<string | null>(null);
  const inFlight = useRef(false);
  useBlockUnload(applying);

  const leave = () => {
    void navigate({ to: "/" });
    return true;
  };
  const form = useRestoreCheck(
    {
      upload: (file) => uploadSetupRestore(file, token),
      check: (uploadId, passphrase) => checkSetupRestore(uploadId, passphrase, token),
      discard: () => discardSetupRestoreUpload(token),
    },
    (err) => (isSetupComplete(err) ? leave() : false),
  );

  async function restore() {
    const checked = form.checked;
    if (!checked || inFlight.current) return;
    inFlight.current = true;
    setApplying(true);
    setFailure(null);
    try {
      await applySetupRestore(checked.uploadId, form.passphrase, token);
    } catch (err) {
      // An answer that says no means nothing changed. A dropped connection is the
      // restart itself, and the wait below finds out how it went.
      if (err instanceof RestoreError) {
        setApplying(false);
        inFlight.current = false;
        if (isSetupComplete(err)) leave();
        else {
          // Too many attempts is said in the wizard's own words, not the server's.
          const reason =
            err.status === 429 || err.type === "rate_limited"
              ? t("restore.errors.rateLimited")
              : err.detail;
          setFailure(
            reason
              ? `${t("restore.failed.nothingChanged")} ${reason}`
              : t("restore.failed.nothingChanged"),
          );
        }
        return;
      }
    }
    setApplying(false);
    setWaiting(true);
    const outcome = await waitForRestoreOutcome(setupProbes());
    setWaiting(false);
    inFlight.current = false;
    if (outcome === "restored") {
      form.reset();
      queryClient.clear();
      await navigate({ to: "/login", search: { notice: "backup-restored" } });
      return;
    }
    setFailure(t(outcome === "timeout" ? "restore.failed.timeout" : "restore.failed.rolledBack"));
  }

  const busy = form.checking || applying;
  return (
    <div className="flex flex-col gap-4">
      {failure ? (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{failure}</AlertDescription>
        </Alert>
      ) : null}
      {form.error ? (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{t(form.error.key, form.error.values)}</AlertDescription>
        </Alert>
      ) : null}
      {form.checked ? (
        <div className="flex flex-col gap-4">
          <BackupSummary summary={form.checked.summary} />
          <div className="flex flex-col gap-2">
            <Button
              type="button"
              className={PRIMARY}
              disabled={busy}
              onClick={() => void restore()}
            >
              {applying ? (
                <>
                  <Spinner aria-hidden="true" />
                  {t("restore.confirm.pending")}
                </>
              ) : (
                t("restore.summary.restore")
              )}
            </Button>
            <Button
              type="button"
              variant="ghost"
              className={PRIMARY}
              disabled={busy}
              onClick={() => {
                setFailure(null);
                form.chooseAnother();
              }}
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
            setFailure(null);
            void form.runCheck();
          }}
        >
          <fieldset disabled={busy} className="contents">
            <RestoreFields
              idPrefix="setup-restore"
              file={form.file}
              fileKey={form.fileKey}
              onFile={form.setFile}
              passphrase={form.passphrase}
              onPassphrase={form.setPassphrase}
              disabled={busy}
            />
            <div className="flex flex-col gap-2">
              <Button
                type="submit"
                variant="secondary"
                className={PRIMARY}
                disabled={!form.file || form.passphrase === ""}
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
              <Button type="button" variant="ghost" className={PRIMARY} onClick={onBack}>
                {t("setup.restore.back")}
              </Button>
            </div>
          </fieldset>
        </form>
      )}
      {waiting ? <RestoringOverlay /> : null}
    </div>
  );
}
