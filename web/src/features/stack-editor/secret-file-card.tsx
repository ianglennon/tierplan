// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Eye, EyeOff } from "lucide-react";
import { type ChangeEvent, useEffect, useRef, useState } from "react";

import { Button } from "@/components/ui/button";
import { useT } from "@/i18n/use-t";

// How long a revealed file's content stays on the page before it is hidden again (UI-SPEC "Design
// System": 60 s).
export const REVEAL_MS = 60_000;

// A secret holds less than 500 KB: the server refuses 512,000 bytes and more, so one byte less is
// the most a chosen file may be (UI-SPEC "File too large").
const MAX_SECRET_FILE_BYTES = 512_000;

export interface SecretFileCardProps {
  // The file's path, which names the card and the reveal.
  path: string;
  // The user holds secrets.reveal, so Show content is offered.
  canReveal: boolean;
  // A Viewer reads the card and can change nothing: no Replace, no Upload.
  readOnly?: boolean;
  // Fetches the saved content. It is a plain function and not a query, so the content never enters
  // the query cache; each call is audited by the server.
  reveal?: () => Promise<string>;
  // Replace content: the page opens an empty editor for the file.
  onReplace: () => void;
  // Upload a file: the page replaces the file's content with this text.
  onUpload: (content: string) => void;
}

// SecretFileCard stands in for the editor of a secret stack file (STK-04, D-02, UI-SPEC "Editor"
// row "Secret file (content hidden)"). It never has the content unless the person holds
// secrets.reveal and asks for it; then the content is held in this component's state alone, shown
// read only as text, and let go of after a minute, when the person hides it, when the file changes
// and with the card. Replace and Upload change the file without ever reading it.
export function SecretFileCard({
  path,
  canReveal,
  readOnly = false,
  reveal,
  onReplace,
  onUpload,
}: SecretFileCardProps) {
  const t = useT();
  const [revealed, setRevealed] = useState<string | null>(null);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [uploadError, setUploadError] = useState<string | null>(null);
  const [notice, setNotice] = useState("");
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const mounted = useRef(true);
  const chooser = useRef<HTMLInputElement>(null);

  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
      clearTimeout(timer.current);
    };
  }, []);

  // Another file is not the one that was revealed.
  // biome-ignore lint/correctness/useExhaustiveDependencies: path is the trigger; the effect only lets go of state
  useEffect(() => {
    clearTimeout(timer.current);
    setRevealed(null);
    setError(null);
    setUploadError(null);
    setNotice("");
  }, [path]);

  function hide(announce: boolean) {
    clearTimeout(timer.current);
    setRevealed(null);
    if (announce) setNotice(t("stackEditor.secretFile.hiddenAgain", { file: path }));
  }

  async function toggleReveal() {
    if (revealed !== null) {
      hide(true);
      return;
    }
    if (!reveal || pending) return;
    setPending(true);
    setError(null);
    setNotice("");
    try {
      const content = await reveal();
      if (!mounted.current) return;
      setRevealed(content);
      clearTimeout(timer.current);
      timer.current = setTimeout(() => hide(true), REVEAL_MS);
    } catch (e) {
      if (!mounted.current) return;
      const status = (e as { status?: number }).status;
      setError(
        status === 403
          ? t("stackEditor.secretFile.revealDenied")
          : t("stackEditor.secretFile.revealFailed"),
      );
    } finally {
      if (mounted.current) setPending(false);
    }
  }

  async function onChosen(e: ChangeEvent<HTMLInputElement>) {
    const file = e.target.files?.[0];
    // The same file can be chosen again.
    e.target.value = "";
    if (!file) return;
    setUploadError(null);
    if (file.size >= MAX_SECRET_FILE_BYTES) {
      setUploadError(t("stackEditor.secretFile.tooBig"));
      return;
    }
    try {
      const text = await file.text();
      if (mounted.current) onUpload(text);
    } catch {
      if (mounted.current) setUploadError(t("stackEditor.secretFile.readFailed"));
    }
  }

  const showButton = canReveal && reveal !== undefined;
  return (
    <div className="flex flex-col gap-3 rounded-lg border p-4">
      <p className="font-medium break-all">{t("stackEditor.secretFile.title", { file: path })}</p>
      <p className="text-sm text-muted-foreground">{t("stackEditor.secretFile.body")}</p>
      <div className="flex flex-wrap items-center gap-2">
        {readOnly ? null : (
          <>
            <Button type="button" variant="outline" className="max-md:h-11" onClick={onReplace}>
              {t("stackEditor.secretFile.replace")}
            </Button>
            <Button
              type="button"
              variant="outline"
              className="max-md:h-11"
              onClick={() => chooser.current?.click()}
            >
              {t("stackEditor.secretFile.upload")}
            </Button>
            <input
              ref={chooser}
              type="file"
              tabIndex={-1}
              aria-hidden="true"
              className="sr-only"
              onChange={(e) => void onChosen(e)}
            />
          </>
        )}
        {showButton ? (
          <Button
            type="button"
            variant="outline"
            className="max-md:h-11"
            aria-pressed={revealed !== null}
            aria-disabled={pending ? true : undefined}
            onClick={() => void toggleReveal()}
          >
            {revealed !== null ? <EyeOff aria-hidden="true" /> : <Eye aria-hidden="true" />}
            {revealed !== null
              ? t("stackEditor.secretFile.hide")
              : t("stackEditor.secretFile.show")}
          </Button>
        ) : null}
      </div>
      {uploadError ? (
        <p role="alert" className="text-sm text-destructive">
          {uploadError}
        </p>
      ) : null}
      {error ? <p className="text-sm text-destructive">{error}</p> : null}
      {revealed !== null ? (
        <textarea
          readOnly
          value={revealed}
          rows={10}
          spellCheck={false}
          autoComplete="off"
          aria-label={t("stackEditor.secretFile.contentLabel", { file: path })}
          className="w-full resize-y rounded-lg border bg-muted p-3 font-mono text-sm break-all"
        />
      ) : null}
      <span role="status" aria-live="polite" className="sr-only">
        {notice}
      </span>
    </div>
  );
}
