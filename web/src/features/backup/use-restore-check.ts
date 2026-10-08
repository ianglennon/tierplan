// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useRef, useState } from "react";

import {
  type RestoreMessage,
  type RestoreSummary,
  type RestoreUpload,
  restoreProblem,
} from "@/api/backup";

// The two calls a check is made of; the Settings page and the setup wizard each
// bring their own routes.
export interface CheckSteps {
  upload: (file: File) => Promise<RestoreUpload>;
  check: (uploadId: string, passphrase: string) => Promise<RestoreSummary>;
}

export interface Checked {
  uploadId: string;
  summary: RestoreSummary;
}

// useRestoreCheck is the state of "choose a file, give its passphrase, check it"
// that both restore paths share. The file and the passphrase live in this state
// and nowhere else: they are cleared by reset (after a restore), by
// chooseAnother, and go with the component when it unmounts. Nothing is uploaded
// before runCheck is called.
export function useRestoreCheck(steps: CheckSteps) {
  const [file, setFile] = useState<File | null>(null);
  const [passphrase, setPassphrase] = useState("");
  const [checking, setChecking] = useState(false);
  const [error, setError] = useState<RestoreMessage | null>(null);
  const [checked, setChecked] = useState<Checked | null>(null);
  // Changing the key makes the browser drop the chosen file from the input.
  const [fileKey, setFileKey] = useState(0);
  const running = useRef(false);
  const latest = useRef(steps);
  latest.current = steps;

  function reset() {
    setFile(null);
    setPassphrase("");
    setChecked(null);
    setError(null);
    setFileKey((k) => k + 1);
  }

  async function runCheck() {
    if (running.current || !file || passphrase === "") return;
    running.current = true;
    setChecking(true);
    setError(null);
    try {
      const upload = await latest.current.upload(file);
      const summary = await latest.current.check(upload.uploadId, passphrase);
      setChecked({ uploadId: upload.uploadId, summary });
    } catch (err) {
      setError(restoreProblem(err));
      return;
    } finally {
      running.current = false;
      setChecking(false);
    }
  }

  return {
    file,
    setFile,
    passphrase,
    setPassphrase,
    checking,
    error,
    checked,
    fileKey,
    runCheck,
    // Back to the empty form: the summary goes, and so do the file and the
    // passphrase it was checked with.
    chooseAnother: reset,
    reset,
  };
}
