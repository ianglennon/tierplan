// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { ChevronRight, TriangleAlert, Upload } from "lucide-react";
import { useId, useMemo, useRef, useState } from "react";

import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Label } from "@/components/ui/label";
import { RadioGroup, RadioGroupItem } from "@/components/ui/radio-group";
import { Spinner } from "@/components/ui/spinner";
import { Textarea } from "@/components/ui/textarea";
import { parseDotenv } from "@/features/stack-editor/dotenv";
import {
  type ImportedVar,
  type ImportMode,
  MAX_FILE_BYTES,
} from "@/features/stack-editor/working-copy";
import { useT } from "@/i18n/use-t";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "max-md:h-11";

export interface ImportEnvDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  // The names the table already holds: an imported name among them replaces its value.
  existing: readonly string[];
  // Brings the variables into the working copy. It saves nothing. A promise keeps the dialog
  // open, with "Importing…", until it settles.
  onImport: (vars: ImportedVar[], mode: ImportMode, secret: boolean) => void | Promise<void>;
}

// ImportEnvDialog is Import .env (STK-04, D-04): paste the text of a .env file or choose one, see
// what it would add and replace, and bring it into the Variables table. The text, the file and the
// choices live in this component's state only: the content is unmounted with the dialog, so
// closing, cancelling and importing each let go of every value. Parsing is in the browser, the
// file is read with the File API, and nothing is uploaded or saved until Save draft.
export function ImportEnvDialog({ open, onOpenChange, existing, onImport }: ImportEnvDialogProps) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="flex max-h-[90svh] flex-col gap-4 overflow-hidden sm:max-w-160">
        <ImportEnvForm
          existing={existing}
          onImport={onImport}
          onClose={() => onOpenChange(false)}
        />
      </DialogContent>
    </Dialog>
  );
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

function ImportEnvForm({
  existing,
  onImport,
  onClose,
}: Pick<ImportEnvDialogProps, "existing" | "onImport"> & { onClose: () => void }) {
  const t = useT();
  const ids = useId();
  const picker = useRef<HTMLInputElement>(null);
  const [text, setText] = useState("");
  const [mode, setMode] = useState<ImportMode>("merge");
  const [secret, setSecret] = useState(false);
  const [fileProblem, setFileProblem] = useState<"readFailed" | "tooBig" | null>(null);
  const [showIgnored, setShowIgnored] = useState(false);
  const [pending, setPending] = useState(false);

  const parsed = useMemo(() => parseDotenv(text), [text]);
  const summary = useMemo(() => {
    const have = new Set(existing);
    const replaced = parsed.vars.filter((v) => have.has(v.name)).length;
    return {
      replaced,
      added: parsed.vars.length - replaced,
      empty: parsed.vars.filter((v) => v.value === "").length,
    };
  }, [parsed, existing]);

  const blank = text.trim() === "";
  const nothing = parsed.vars.length === 0;
  const reason = blank
    ? t("stackImport.dialog.reason.empty")
    : nothing
      ? t("stackImport.dialog.reason.nothing")
      : null;

  async function choose(file: File | undefined) {
    if (!file) return;
    setFileProblem(null);
    if (file.size > MAX_FILE_BYTES) {
      setFileProblem("tooBig");
      return;
    }
    try {
      setText(await readText(file));
    } catch {
      setFileProblem("readFailed");
    }
  }

  async function submit() {
    if (nothing || pending) return;
    setPending(true);
    try {
      await onImport(parsed.vars, mode, secret);
      onClose();
    } finally {
      setPending(false);
    }
  }

  return (
    <>
      <DialogHeader>
        <DialogTitle>{t("stackImport.dialog.title")}</DialogTitle>
        <DialogDescription>{t("stackImport.dialog.body")}</DialogDescription>
      </DialogHeader>
      <form
        id={`${ids}-form`}
        noValidate
        onSubmit={(e) => {
          e.preventDefault();
          void submit();
        }}
        className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto pr-1"
      >
        <div className="flex flex-col gap-2">
          <Label htmlFor={`${ids}-text`}>{t("stackImport.dialog.text")}</Label>
          <Textarea
            id={`${ids}-text`}
            value={text}
            onChange={(e) => {
              setText(e.target.value);
              setFileProblem(null);
            }}
            rows={10}
            autoComplete="off"
            autoCapitalize="off"
            autoCorrect="off"
            spellCheck={false}
            disabled={pending}
            className="max-h-72 min-h-0 field-sizing-fixed overflow-y-auto font-mono md:text-xs"
          />
          <div className="flex flex-wrap items-center gap-2">
            <Button
              type="button"
              variant="outline"
              size="sm"
              className={TOUCH_HEIGHT}
              disabled={pending}
              onClick={() => picker.current?.click()}
            >
              <Upload aria-hidden="true" />
              {t("stackImport.dialog.chooseFile")}
            </Button>
            <input
              ref={picker}
              type="file"
              className="sr-only"
              tabIndex={-1}
              aria-label={t("stackImport.dialog.chooseFile")}
              data-testid="import-env-file"
              onChange={(e) => {
                const file = e.target.files?.[0];
                // Let go of the choice at once, so choosing the same file again is a change.
                e.target.value = "";
                void choose(file);
              }}
            />
            {fileProblem ? (
              <p role="alert" className="text-xs text-destructive">
                {t(`stackImport.dialog.${fileProblem}`)}
              </p>
            ) : null}
          </div>
        </div>

        <fieldset className="flex flex-col gap-2" disabled={pending}>
          <legend className="mb-1 text-sm font-medium">{t("stackImport.dialog.mode.label")}</legend>
          <RadioGroup
            value={mode}
            onValueChange={(next) => setMode(next as ImportMode)}
            aria-label={t("stackImport.dialog.mode.label")}
          >
            {(["merge", "replace"] as const).map((value) => (
              <div key={value} className="flex items-center gap-2 max-md:min-h-11">
                <RadioGroupItem value={value} id={`${ids}-mode-${value}`} />
                <Label htmlFor={`${ids}-mode-${value}`}>
                  {t(`stackImport.dialog.mode.${value}`)}
                </Label>
              </div>
            ))}
          </RadioGroup>
          <div className="flex items-center gap-2 max-md:min-h-11">
            <Checkbox
              id={`${ids}-secret`}
              checked={secret}
              onCheckedChange={(next) => setSecret(next === true)}
            />
            <Label htmlFor={`${ids}-secret`}>{t("stackImport.dialog.secret")}</Label>
          </div>
        </fieldset>

        <div className="flex flex-col gap-2" aria-live="polite" data-testid="import-env-preview">
          {blank ? null : nothing ? (
            <p className="text-sm text-destructive">{t("stackImport.dialog.noneFound")}</p>
          ) : (
            <p className="text-sm break-all text-muted-foreground">
              {t("stackImport.dialog.summary", {
                count: parsed.ignored.length,
                new: summary.added,
                replaced: summary.replaced,
              })}
            </p>
          )}
          {summary.empty > 0 ? (
            <Alert variant="warning" role="status">
              <TriangleAlert aria-hidden="true" />
              <AlertDescription>
                {t("stackImport.dialog.noValue", { count: summary.empty })}
              </AlertDescription>
            </Alert>
          ) : null}
          {parsed.ignored.length > 0 ? (
            <Collapsible open={showIgnored} onOpenChange={setShowIgnored}>
              <CollapsibleTrigger asChild>
                <Button type="button" variant="ghost" size="sm" className={`-ml-2 ${TOUCH_HEIGHT}`}>
                  <ChevronRight
                    aria-hidden="true"
                    className={
                      showIgnored ? "rotate-90 transition-transform" : "transition-transform"
                    }
                  />
                  {showIgnored
                    ? t("stackImport.dialog.ignored.hide")
                    : t("stackImport.dialog.ignored.show")}
                </Button>
              </CollapsibleTrigger>
              <CollapsibleContent>
                <ul
                  aria-label={t("stackImport.dialog.ignored.list")}
                  className="max-h-48 overflow-y-auto rounded-md border p-2"
                >
                  {parsed.ignored.map((line) => (
                    <li key={line.line} className="flex flex-col gap-0.5 py-1 text-xs">
                      <span className="text-muted-foreground">
                        {t("stackImport.dialog.ignored.line", { line: line.line })}
                        {" · "}
                        {t(`stackImport.dialog.ignored.reason.${line.reason}`)}
                      </span>
                      {line.text === "" ? null : (
                        <code className="font-mono break-all">{line.text}</code>
                      )}
                    </li>
                  ))}
                </ul>
              </CollapsibleContent>
            </Collapsible>
          ) : null}
        </div>
      </form>
      <DialogFooter className="items-center">
        {reason ? (
          <p id={`${ids}-reason`} className="text-xs text-muted-foreground sm:mr-auto">
            {reason}
          </p>
        ) : null}
        <Button
          type="button"
          variant="outline"
          className={TOUCH_HEIGHT}
          disabled={pending}
          onClick={onClose}
        >
          {t("stackImport.dialog.cancel")}
        </Button>
        <Button
          type="submit"
          form={`${ids}-form`}
          className={TOUCH_HEIGHT}
          disabled={nothing || pending}
          aria-describedby={reason ? `${ids}-reason` : undefined}
        >
          {pending ? <Spinner aria-hidden="true" /> : null}
          {pending ? t("stackImport.dialog.pending") : t("stackImport.dialog.submit")}
        </Button>
      </DialogFooter>
    </>
  );
}
