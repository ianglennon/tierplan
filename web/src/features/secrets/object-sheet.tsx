// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useEffect, useId, useMemo, useRef, useState } from "react";

import { SecretRequestError, useCreateSecret } from "@/api/secrets";
import { entriesOf, KeyValueEditor, type KeyValueRow } from "@/components/key-value-editor";
import { Alert, AlertDescription } from "@/components/ui/alert";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import { Field, FieldDescription, FieldError, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetFooter,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import { Spinner } from "@/components/ui/spinner";
import {
  EMPTY_VALUE,
  type ObjectValue,
  type ValueProblem,
  ValueSourceField,
  valueBytes,
  valueProblem,
} from "@/features/secrets/value-source-field";
import { useT } from "@/i18n/use-t";
import { toastSuccess } from "@/lib/toast";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

// A secret's name: 1 to 64 letters, numbers, dots, dashes and underscores, starting and ending
// with a letter or a number. The server checks it again.
const NAME_PATTERN = /^[A-Za-z0-9](?:[A-Za-z0-9._-]{0,62}[A-Za-z0-9])?$/;

// toBase64 is the standard base64 of bytes, built in pieces so a large value does not overflow
// the call stack.
function toBase64(bytes: Uint8Array): string {
  const piece = 0x8000;
  let binary = "";
  for (let at = 0; at < bytes.length; at += piece) {
    binary += String.fromCharCode(...bytes.subarray(at, at + piece));
  }
  return btoa(binary);
}

interface ObjectSheetProps {
  envId: string;
  // A secret now; the config sheet comes with the configs pages.
  kind: "secret";
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

// ObjectSheet is the right-hand sheet that creates a secret (UI-SPEC "Secrets and configs"): a
// name that cannot change later, labels, and a value typed in a masked field or read from a file.
// The value lives in this component's state until it is sent, once, as base64; it is cleared when
// the sheet closes, is cancelled or discarded, and when the create succeeds, and it is in no toast,
// storage, address or cache. Closing with something entered asks first, and nothing closes the
// sheet while the request is out.
export function ObjectSheet({ envId, open, onOpenChange }: ObjectSheetProps) {
  const t = useT();
  const ids = useId();
  const mutation = useCreateSecret(envId);
  const [name, setName] = useState("");
  const [labels, setLabels] = useState<KeyValueRow[]>([]);
  const [value, setValue] = useState<ObjectValue>(EMPTY_VALUE);
  const [submitted, setSubmitted] = useState(false);
  const [serverName, setServerName] = useState<string | null>(null);
  const [serverValue, setServerValue] = useState<ValueProblem | null>(null);
  const [formError, setFormError] = useState<string | null>(null);
  const [discarding, setDiscarding] = useState(false);
  // A second press can land before the pending state has rendered.
  const inFlight = useRef(false);
  const pending = mutation.isPending;

  const trimmed = name.trim();
  const nameProblem: "required" | "invalid" | undefined =
    trimmed === "" ? "required" : NAME_PATTERN.test(trimmed) ? undefined : "invalid";
  const typedValueProblem = useMemo(() => valueProblem("secret", value), [value]);
  const dirty =
    name !== "" ||
    labels.some((r) => r.key !== "" || r.value !== "") ||
    value.text !== "" ||
    value.file !== null;

  function reset() {
    setName("");
    setLabels([]);
    setValue(EMPTY_VALUE);
    setSubmitted(false);
    setServerName(null);
    setServerValue(null);
    setFormError(null);
    setDiscarding(false);
    mutation.reset();
  }

  // Whatever way the sheet is closed, even by its owner, nothing of it is kept.
  // biome-ignore lint/correctness/useExhaustiveDependencies: reset is recreated every render and only clears state
  useEffect(() => {
    if (!open) reset();
  }, [open]);

  function close() {
    reset();
    onOpenChange(false);
  }

  // Escape, the overlay, the X and Cancel all come through here. While a create is out nothing
  // closes the sheet: unmounting the form would drop the outcome, and the person would never see
  // the toast or the server's refusal.
  function requestClose() {
    if (pending || inFlight.current) return;
    if (dirty) setDiscarding(true);
    else close();
  }

  function place(err: SecretRequestError) {
    switch (err.type) {
      case "name_taken":
        setServerName(t("secrets.errors.nameTaken", { name: trimmed }));
        return;
      case "name_invalid":
        setServerName(t("secrets.errors.nameInvalid"));
        return;
      case "value_required":
        setServerValue("empty");
        return;
      case "value_too_large":
        setServerValue("tooBig");
        return;
      case "labels_invalid":
        setFormError(t("secrets.errors.labels"));
        return;
      case "host_read_only":
        setFormError(t("secrets.errors.hostReadOnly"));
        return;
      case "no_manager_connected":
        setFormError(t("secrets.errors.noManager"));
        return;
      case "not_a_swarm":
        setFormError(t("secrets.errors.notASwarm"));
        return;
      default:
        setFormError(
          err.status === 403 ? t("secrets.errors.permission") : t("secrets.errors.generic"),
        );
    }
  }

  async function submit() {
    if (pending || inFlight.current) return;
    setSubmitted(true);
    setServerName(null);
    setServerValue(null);
    setFormError(null);
    const bytes = valueBytes(value);
    if (nameProblem || typedValueProblem || bytes === null) return;
    inFlight.current = true;
    const entered = entriesOf(labels);
    try {
      await mutation.mutateAsync({
        name: trimmed,
        ...(Object.keys(entered).length > 0 ? { labels: entered } : {}),
        data: toBase64(bytes),
        source: value.source === "file" ? "file" : "typed",
      });
      toastSuccess(t("secrets.sheet.done", { name: trimmed }));
      close();
    } catch (e) {
      place(e instanceof SecretRequestError ? e : new SecretRequestError(0));
    } finally {
      inFlight.current = false;
    }
  }

  const nameError = serverName
    ? serverName
    : submitted && nameProblem
      ? t(nameProblem === "required" ? "secrets.errors.nameRequired" : "secrets.errors.nameInvalid")
      : undefined;
  const valueError = serverValue ?? (submitted ? typedValueProblem : undefined);

  return (
    <>
      <Sheet
        open={open}
        onOpenChange={(next) => {
          if (!next) requestClose();
        }}
      >
        <SheetContent
          className="w-full gap-0 data-[side=right]:w-full data-[side=right]:sm:max-w-160"
          aria-describedby={`${ids}-description`}
        >
          <SheetHeader className="border-b pe-12">
            <SheetTitle className="break-words">{t("secrets.sheet.title")}</SheetTitle>
            <SheetDescription id={`${ids}-description`}>
              {t("secrets.sheet.description")}
            </SheetDescription>
          </SheetHeader>
          <form
            noValidate
            className="flex min-h-0 flex-1 flex-col gap-6 overflow-y-auto px-4 py-4"
            onSubmit={(e) => {
              e.preventDefault();
              void submit();
            }}
          >
            {formError ? (
              <Alert variant="destructive" role="alert">
                <AlertDescription>{formError}</AlertDescription>
              </Alert>
            ) : null}

            <Field data-invalid={nameError ? true : undefined}>
              <FieldLabel htmlFor={`${ids}-name`}>{t("secrets.sheet.name.label")}</FieldLabel>
              <Input
                id={`${ids}-name`}
                value={name}
                maxLength={64}
                autoComplete="off"
                autoCapitalize="off"
                autoCorrect="off"
                spellCheck={false}
                disabled={pending}
                aria-invalid={nameError ? true : undefined}
                aria-describedby={`${ids}-name-helper${nameError ? ` ${ids}-name-error` : ""}`}
                className={`font-mono ${TOUCH_HEIGHT}`}
                onChange={(e) => {
                  setName(e.target.value);
                  setServerName(null);
                }}
              />
              <FieldDescription id={`${ids}-name-helper`}>
                {t("secrets.sheet.name.helper")}
              </FieldDescription>
              {nameError ? <FieldError id={`${ids}-name-error`}>{nameError}</FieldError> : null}
            </Field>

            <Field>
              <FieldLabel>{t("secrets.sheet.labels.label")}</FieldLabel>
              <KeyValueEditor
                value={labels}
                onChange={setLabels}
                labels={{
                  add: t("secrets.sheet.labels.add"),
                  key: t("secrets.sheet.labels.key"),
                  value: t("secrets.sheet.labels.value"),
                  remove: (key) => t("secrets.sheet.labels.remove", { key }),
                }}
              />
            </Field>

            <ValueSourceField
              kind="secret"
              value={value}
              disabled={pending}
              problem={valueError}
              onChange={(next) => {
                setValue(next);
                setServerValue(null);
              }}
            />
            <button type="submit" hidden aria-hidden="true" tabIndex={-1} />
          </form>
          <SheetFooter className="sticky bottom-0 mt-0 flex-row justify-end border-t bg-popover">
            <Button
              type="button"
              variant="outline"
              className={TOUCH_HEIGHT}
              disabled={pending}
              onClick={requestClose}
            >
              {t("secrets.sheet.cancel")}
            </Button>
            <Button
              type="button"
              className={`${TOUCH_HEIGHT} aria-disabled:cursor-not-allowed aria-disabled:opacity-50`}
              aria-disabled={pending}
              onClick={() => void submit()}
            >
              {pending ? <Spinner aria-hidden="true" /> : null}
              {t("secrets.sheet.submit")}
            </Button>
          </SheetFooter>
        </SheetContent>
      </Sheet>

      <AlertDialog open={discarding} onOpenChange={setDiscarding}>
        <AlertDialogContent className="sm:max-w-120">
          <AlertDialogHeader>
            <AlertDialogTitle>{t("secrets.sheet.discard.title")}</AlertDialogTitle>
            <AlertDialogDescription>{t("secrets.sheet.discard.body")}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel autoFocus>{t("secrets.sheet.discard.keep")}</AlertDialogCancel>
            <AlertDialogAction onClick={close}>
              {t("secrets.sheet.discard.discard")}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  );
}
