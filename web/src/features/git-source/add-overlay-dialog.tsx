// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Upload } from "lucide-react";
import { type FormEvent, useId, useRef, useState } from "react";

import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import { useT } from "@/i18n/use-t";

// An overlay file holds at most 1 MiB; the server refuses more (file_too_large).
const MAX_BYTES = 1_048_576;

type PathProblem = "invalid" | "inRepo" | "taken";

// pathProblem judges a path as the server does: relative to the stack folder, no leading, trailing
// or doubled slash, no "." or ".." name, no backslash or colon. It also refuses a path the folder
// already holds and one that is already an overlay file. The server has the last word: a repository
// path deeper than the folder's own listing is reported by the check as a clash.
function pathProblem(
  path: string,
  repoPaths: readonly string[],
  overlays: readonly string[],
): PathProblem | null {
  if (path === "" || path.startsWith("/") || /[\\:]/.test(path)) return "invalid";
  if (path.split("/").some((segment) => segment === "" || segment === "." || segment === "..")) {
    return "invalid";
  }
  if (repoPaths.includes(path)) return "inRepo";
  if (overlays.includes(path)) return "taken";
  return null;
}

// readText reads a chosen file into memory. Nothing is uploaded.
function readText(file: File): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => resolve(String(reader.result ?? ""));
    reader.onerror = () => reject(reader.error);
    reader.readAsText(file);
  });
}

export interface AddOverlayDialogProps {
  // The paths, relative to the stack folder, the repository holds there: an overlay file can only
  // fill a path that is not one of them (D-09).
  existingRepoPaths: readonly string[];
  // The paths already added as overlay files.
  overlays: readonly string[];
  // Hands over the new overlay file. The caller keeps it in memory until the stack is created.
  onAdd: (overlay: { path: string; content: string }) => void;
  onClose: () => void;
  // Defaults to "Add an overlay file".
  title?: string;
  // The path to start with, as when the check named a missing file.
  initialPath?: string;
}

// AddOverlayDialog is the Add overlay file dialog (UI-SPEC, D-09): a path inside the stack folder
// for a file the repository does not have, and its contents, which are write-only. The dialog is
// rendered while it is open, so closing, cancelling and adding each let go of the contents; they
// are in this component's state and nowhere else (not storage, not the address).
export function AddOverlayDialog({
  existingRepoPaths,
  overlays,
  onAdd,
  onClose,
  title,
  initialPath = "",
}: AddOverlayDialogProps) {
  const t = useT();
  const ids = useId();
  const picker = useRef<HTMLInputElement>(null);
  const [path, setPath] = useState(initialPath);
  const [content, setContent] = useState("");
  const [pathError, setPathError] = useState<PathProblem | null>(null);
  const [contentError, setContentError] = useState<"tooBig" | "readFailed" | null>(null);

  async function choose(file: File | undefined) {
    if (!file) return;
    setContentError(null);
    if (file.size > MAX_BYTES) {
      setContentError("tooBig");
      return;
    }
    try {
      setContent(await readText(file));
    } catch {
      setContentError("readFailed");
    }
  }

  function submit(e: FormEvent) {
    e.preventDefault();
    const next = path.trim();
    const problem = pathProblem(next, existingRepoPaths, overlays);
    setPathError(problem);
    if (problem) return;
    if (new TextEncoder().encode(content).length > MAX_BYTES) {
      setContentError("tooBig");
      return;
    }
    onAdd({ path: next, content });
    onClose();
  }

  const pathId = `${ids}-path`;
  const errorId = `${ids}-path-error`;
  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open) onClose();
      }}
    >
      <DialogContent className="flex max-h-[90svh] flex-col gap-4 overflow-hidden sm:max-w-140">
        <DialogHeader>
          <DialogTitle>{title ?? t("gitSource.overlay.dialog.title")}</DialogTitle>
          <DialogDescription>{t("gitSource.overlay.dialog.body")}</DialogDescription>
        </DialogHeader>
        <form
          noValidate
          onSubmit={submit}
          className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto pr-1"
        >
          <div className="flex flex-col gap-2">
            <Label htmlFor={pathId}>{t("gitSource.overlay.dialog.path.label")}</Label>
            <Input
              id={pathId}
              value={path}
              placeholder={t("gitSource.overlay.dialog.path.placeholder")}
              aria-invalid={pathError ? true : undefined}
              aria-describedby={pathError ? errorId : `${pathId}-helper`}
              autoComplete="off"
              autoCapitalize="off"
              autoCorrect="off"
              spellCheck={false}
              onChange={(e) => {
                setPath(e.target.value);
                setPathError(null);
              }}
              className="font-mono max-md:h-11"
            />
            {pathError ? (
              <p id={errorId} role="alert" className="text-xs break-words text-destructive">
                {t(`gitSource.overlay.dialog.path.${pathError}`, { path: path.trim() })}
              </p>
            ) : (
              <p id={`${pathId}-helper`} className="text-xs text-muted-foreground">
                {t("gitSource.overlay.dialog.path.helper")}
              </p>
            )}
          </div>
          <div className="flex flex-col gap-2">
            <Label htmlFor={`${ids}-content`}>{t("gitSource.overlay.dialog.contents")}</Label>
            <Textarea
              id={`${ids}-content`}
              value={content}
              rows={8}
              autoComplete="off"
              autoCapitalize="off"
              autoCorrect="off"
              spellCheck={false}
              onChange={(e) => {
                setContent(e.target.value);
                setContentError(null);
              }}
              className="max-h-72 min-h-0 field-sizing-fixed overflow-y-auto font-mono md:text-xs"
            />
            <div className="flex flex-wrap items-center gap-2">
              <Button
                type="button"
                variant="outline"
                size="sm"
                className="max-md:h-11"
                onClick={() => picker.current?.click()}
              >
                <Upload aria-hidden="true" />
                {t("gitSource.overlay.dialog.chooseFile")}
              </Button>
              <input
                ref={picker}
                type="file"
                className="sr-only"
                tabIndex={-1}
                aria-hidden="true"
                data-testid="overlay-file"
                onChange={(e) => {
                  const file = e.target.files?.[0];
                  // Let go of the choice at once, so choosing the same file again is a change.
                  e.target.value = "";
                  void choose(file);
                }}
              />
              {contentError ? (
                <p role="alert" className="text-xs text-destructive">
                  {t(`gitSource.overlay.dialog.${contentError}`)}
                </p>
              ) : null}
            </div>
          </div>
          <DialogFooter>
            <Button type="button" variant="outline" className="max-md:h-11" onClick={onClose}>
              {t("gitSource.overlay.dialog.cancel")}
            </Button>
            <Button type="submit" className="max-md:h-11">
              {t("gitSource.overlay.dialog.submit")}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
