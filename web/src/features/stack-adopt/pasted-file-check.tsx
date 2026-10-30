// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { CircleCheck, CircleX, TriangleAlert } from "lucide-react";
import { useId, useRef, useState } from "react";

import { type AdoptCheck, MAX_PASTED_BYTES } from "@/api/stack-adopt";
import { Button } from "@/components/ui/button";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import { useT } from "@/i18n/use-t";

interface PastedFileCheckProps {
  // The pasted text. It is the page's own state: this component keeps no copy.
  value: string;
  onChange: (value: string) => void;
  // The check of the file in its current state, once it has settled; absent while it waits, runs or failed.
  check?: AdoptCheck;
  // True from a change until the check of it answers.
  checking: boolean;
  // Switches the adoption to the rebuild from what runs.
  onUseRebuild: () => void;
  // Takes the person to the list of differences.
  onShowDifferences: () => void;
}

// How many settings a pasted file would change on the first deploy: the settings of a changed service, one for a
// service that is added, removed or recreated.
function differenceCount(check: AdoptCheck): number {
  return (check.gaps ?? []).reduce(
    (n, g) => n + (g.change === "changed" ? Math.max(g.settings, 1) : 1),
    0,
  );
}

// PastedFileCheck is the paste field of the Adopt page (UI-SPEC "Paste field", "Paste result", D-12): the original
// compose file, which is untrusted text that may hold secrets. It sits in a mono textarea of 12 rows that scrolls
// inside itself, with autocomplete off and spell checking off; "Choose file" reads a file of at most 1 MiB into memory;
// "Remove pasted file" empties it. The result line is a polite live region that is filled in once the check of the
// current text has settled: it matches what runs, or it differs and the person may adopt it as it is or use the
// rebuild. The text is never written anywhere but into the page's state.
export function PastedFileCheck({
  value,
  onChange,
  check,
  checking,
  onUseRebuild,
  onShowDifferences,
}: PastedFileCheckProps) {
  const t = useT();
  const ids = useId();
  const fileInput = useRef<HTMLInputElement>(null);
  const [fileError, setFileError] = useState<string | null>(null);
  const tooLarge = value.length > MAX_PASTED_BYTES;
  const problems = (check?.problems ?? []).filter((p) => p.severity === "error");

  async function chooseFile(file: File | undefined) {
    if (fileInput.current) fileInput.current.value = "";
    if (!file) return;
    if (file.size > MAX_PASTED_BYTES) {
      setFileError(t("stackAdoptPaste.file.tooLarge"));
      return;
    }
    try {
      onChange(await file.text());
      setFileError(null);
    } catch {
      setFileError(t("stackAdoptPaste.file.readFailed"));
    }
  }

  const settled = check && !checking && value.trim() !== "" && !tooLarge ? check : undefined;
  const differs = settled && !settled.blocked && !settled.matches;
  return (
    <div className="flex min-w-0 flex-col gap-2" data-slot="pasted-file">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <Label htmlFor={`${ids}-file`}>{t("stackAdoptPaste.file.label")}</Label>
        <div className="flex flex-wrap items-center gap-2">
          {/* The system's file chooser opens from the button; the input is for assistive technology and tests. */}
          <input
            ref={fileInput}
            type="file"
            tabIndex={-1}
            aria-label={t("stackAdoptPaste.file.input")}
            className="sr-only"
            onChange={(e) => void chooseFile(e.target.files?.[0])}
          />
          <Button
            type="button"
            variant="ghost"
            size="sm"
            className="max-md:h-11"
            onClick={() => fileInput.current?.click()}
          >
            {t("stackAdoptPaste.file.choose")}
          </Button>
          {value !== "" ? (
            <Button
              type="button"
              variant="ghost"
              size="sm"
              className="max-md:h-11"
              onClick={() => {
                onChange("");
                setFileError(null);
              }}
            >
              {t("stackAdoptPaste.file.remove")}
            </Button>
          ) : null}
        </div>
      </div>
      <Textarea
        id={`${ids}-file`}
        value={value}
        onChange={(e) => {
          onChange(e.target.value);
          setFileError(null);
        }}
        rows={12}
        wrap="off"
        autoComplete="off"
        autoCapitalize="off"
        autoCorrect="off"
        spellCheck={false}
        placeholder={t("stackAdoptPaste.file.placeholder")}
        aria-describedby={`${ids}-help`}
        className="field-sizing-fixed overflow-auto font-mono text-sm leading-6 whitespace-pre md:text-sm"
      />
      <p id={`${ids}-help`} className="text-sm text-muted-foreground">
        {t("stackAdoptPaste.file.help")}
      </p>
      {fileError || tooLarge ? (
        <p role="alert" className="flex items-start gap-2 text-sm text-status-offline-text">
          <CircleX aria-hidden="true" className="mt-0.5 size-4 shrink-0" />
          <span className="min-w-0 break-words">
            {fileError ?? t("stackAdoptPaste.file.tooLarge")}
          </span>
        </p>
      ) : null}

      {/* The one announcement of a settled result for this file. */}
      <div role="status" aria-live="polite" className="flex flex-col gap-2 text-sm">
        {settled?.matches ? (
          <p className="flex items-start gap-2 font-medium">
            <CircleCheck aria-hidden="true" className="mt-0.5 size-4 shrink-0 text-status-online" />
            <span className="min-w-0 break-words">{t("stackAdoptPaste.result.matches")}</span>
          </p>
        ) : null}
        {differs ? (
          <>
            <p className="flex items-start gap-2 font-medium">
              <TriangleAlert
                aria-hidden="true"
                className="mt-0.5 size-4 shrink-0 text-status-warning-text"
              />
              <span className="min-w-0 break-words">
                {t("stackAdoptPaste.result.differs", { count: differenceCount(settled) })}
              </span>
            </p>
            <div className="flex flex-wrap gap-2 ps-6">
              <Button
                type="button"
                variant="outline"
                size="sm"
                className="max-md:h-11"
                onClick={onShowDifferences}
              >
                {t("stackAdoptPaste.result.show")}
              </Button>
              <Button
                type="button"
                variant="outline"
                size="sm"
                className="max-md:h-11"
                onClick={onUseRebuild}
              >
                {t("stackAdoptPaste.result.useRebuild")}
              </Button>
            </div>
          </>
        ) : null}
      </div>

      {settled?.blocked && problems.length > 0 ? (
        <div className="flex flex-col gap-2" data-slot="pasted-problems">
          <p className="flex items-start gap-2 text-sm font-medium">
            <CircleX
              aria-hidden="true"
              className="mt-0.5 size-4 shrink-0 text-status-offline-text"
            />
            <span className="min-w-0 break-words">{t("stackAdoptPaste.problems.title")}</span>
          </p>
          <ul className="flex max-h-48 flex-col gap-1 overflow-y-auto ps-6 text-sm">
            {problems.map((p) => (
              <li key={`${p.line}:${p.col}:${p.code}:${p.message}`} className="min-w-0 break-words">
                {t("stackAdoptPaste.problems.line", { line: p.line, message: p.message })}
              </li>
            ))}
          </ul>
          <div className="ps-6">
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="max-md:h-11"
              onClick={onUseRebuild}
            >
              {t("stackAdoptPaste.result.useRebuild")}
            </Button>
          </div>
        </div>
      ) : null}
    </div>
  );
}
