// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useEffect, useId, useMemo, useRef, useState } from "react";

import { ConfigRequestError, useCreateConfig } from "@/api/configs";
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
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
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
  type ObjectKind,
  type ObjectValue,
  toBase64,
  type ValueProblem,
  ValueSourceField,
  valueBytes,
  valueProblem,
} from "@/features/secrets/value-source-field";
import { useT } from "@/i18n/use-t";
import { toastSuccess } from "@/lib/toast";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

// A secret's or config's name: 1 to 64 letters, numbers, dots, dashes and underscores, starting and ending
// with a letter or a number. The server checks it again.
export const NAME_PATTERN = /^[A-Za-z0-9](?:[A-Za-z0-9._-]{0,62}[A-Za-z0-9])?$/;

// What the swarm fills a config in as when a task starts: nothing, or a Go template.
type Templating = "none" | "golang";

interface ObjectSheetProps {
  envId: string;
  // What is created: a secret (a masked value, 500 KB) or a config (a text area, an optional Go
  // template, 1000 KB). They share the sheet, the name, the labels and the value field.
  kind: ObjectKind;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

// ObjectSheet is the right-hand sheet that creates a secret or a config (UI-SPEC "Secrets and
// configs"): a name that cannot change later, labels, and a value typed or read from a file; a
// config also has its content in a text area and an optional Go template. The value lives in this
// component's state until it is sent, once, as base64; it is cleared when the sheet closes, is
// cancelled or discarded, and when the create succeeds, and it is in no toast, storage, address or
// cache. Closing with something entered asks first, and nothing closes the sheet while the request
// is out.
export function ObjectSheet({ envId, kind, open, onOpenChange }: ObjectSheetProps) {
  const t = useT();
  const ids = useId();
  // The words of the sheet are the secrets' or the configs'; the structure is the same.
  const ns = kind === "config" ? "configs" : "secrets";
  const secretMutation = useCreateSecret(envId);
  const configMutation = useCreateConfig(envId);
  const [name, setName] = useState("");
  const [labels, setLabels] = useState<KeyValueRow[]>([]);
  const [value, setValue] = useState<ObjectValue>(EMPTY_VALUE);
  const [submitted, setSubmitted] = useState(false);
  const [serverName, setServerName] = useState<string | null>(null);
  const [serverValue, setServerValue] = useState<ValueProblem | null>(null);
  const [templating, setTemplating] = useState<Templating>("none");
  const [serverTemplating, setServerTemplating] = useState(false);
  const [formError, setFormError] = useState<string | null>(null);
  const [discarding, setDiscarding] = useState(false);
  // A second press can land before the pending state has rendered.
  const inFlight = useRef(false);
  const pending = secretMutation.isPending || configMutation.isPending;

  const trimmed = name.trim();
  const nameProblem: "required" | "invalid" | undefined =
    trimmed === "" ? "required" : NAME_PATTERN.test(trimmed) ? undefined : "invalid";
  const typedValueProblem = useMemo(() => valueProblem(kind, value), [kind, value]);
  const dirty =
    name !== "" ||
    labels.some((r) => r.key !== "" || r.value !== "") ||
    value.text !== "" ||
    value.file !== null ||
    templating !== "none";

  function reset() {
    setName("");
    setLabels([]);
    setValue(EMPTY_VALUE);
    setSubmitted(false);
    setServerName(null);
    setServerValue(null);
    setTemplating("none");
    setServerTemplating(false);
    setFormError(null);
    setDiscarding(false);
    secretMutation.reset();
    configMutation.reset();
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

  function place(err: SecretRequestError | ConfigRequestError) {
    switch (err.type) {
      case "name_taken":
        setServerName(t(`${ns}.errors.nameTaken`, { name: trimmed }));
        return;
      case "name_invalid":
        setServerName(t(`${ns}.errors.nameInvalid`));
        return;
      case "value_required":
        setServerValue("empty");
        return;
      case "value_too_large":
        setServerValue("tooBig");
        return;
      case "templating_invalid":
        setServerTemplating(true);
        return;
      case "labels_invalid":
        setFormError(t(`${ns}.errors.labels`));
        return;
      case "host_read_only":
        setFormError(t(`${ns}.errors.hostReadOnly`));
        return;
      case "no_manager_connected":
        setFormError(t(`${ns}.errors.noManager`));
        return;
      case "not_a_swarm":
        setFormError(t(`${ns}.errors.notASwarm`));
        return;
      default:
        setFormError(err.status === 403 ? t(`${ns}.errors.permission`) : t(`${ns}.errors.generic`));
    }
  }

  async function submit() {
    if (pending || inFlight.current) return;
    setSubmitted(true);
    setServerName(null);
    setServerValue(null);
    setServerTemplating(false);
    setFormError(null);
    const bytes = valueBytes(value);
    if (nameProblem || typedValueProblem || bytes === null) return;
    inFlight.current = true;
    const entered = entriesOf(labels);
    try {
      const body = {
        name: trimmed,
        ...(Object.keys(entered).length > 0 ? { labels: entered } : {}),
        data: toBase64(bytes),
        source: value.source === "file" || value.fromFile ? ("file" as const) : ("typed" as const),
      };
      if (kind === "config") {
        await configMutation.mutateAsync({
          ...body,
          ...(templating === "golang" ? { templating } : {}),
        });
      } else {
        await secretMutation.mutateAsync(body);
      }
      toastSuccess(t(`${ns}.sheet.done`, { name: trimmed }));
      close();
    } catch (e) {
      place(
        e instanceof SecretRequestError || e instanceof ConfigRequestError
          ? e
          : new SecretRequestError(0),
      );
    } finally {
      inFlight.current = false;
    }
  }

  const nameError = serverName
    ? serverName
    : submitted && nameProblem
      ? t(nameProblem === "required" ? `${ns}.errors.nameRequired` : `${ns}.errors.nameInvalid`)
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
            <SheetTitle className="break-words">{t(`${ns}.sheet.title`)}</SheetTitle>
            <SheetDescription id={`${ids}-description`}>
              {t(`${ns}.sheet.description`)}
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
              <FieldLabel htmlFor={`${ids}-name`}>{t(`${ns}.sheet.name.label`)}</FieldLabel>
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
                {t(`${ns}.sheet.name.helper`)}
              </FieldDescription>
              {nameError ? <FieldError id={`${ids}-name-error`}>{nameError}</FieldError> : null}
            </Field>

            <Field>
              <FieldLabel>{t(`${ns}.sheet.labels.label`)}</FieldLabel>
              <KeyValueEditor
                value={labels}
                onChange={setLabels}
                labels={{
                  add: t(`${ns}.sheet.labels.add`),
                  key: t(`${ns}.sheet.labels.key`),
                  value: t(`${ns}.sheet.labels.value`),
                  remove: (key) => t(`${ns}.sheet.labels.remove`, { key }),
                }}
              />
            </Field>

            <ValueSourceField
              kind={kind}
              value={value}
              disabled={pending}
              problem={valueError}
              onChange={(next) => {
                setValue(next);
                setServerValue(null);
              }}
            />
            {kind === "config" ? (
              <Field data-invalid={serverTemplating ? true : undefined}>
                <FieldLabel htmlFor={`${ids}-templating`}>
                  {t("configs.sheet.templating.label")}
                </FieldLabel>
                <Select
                  value={templating}
                  disabled={pending}
                  onValueChange={(next) => {
                    setTemplating(next === "golang" ? "golang" : "none");
                    setServerTemplating(false);
                  }}
                >
                  <SelectTrigger
                    id={`${ids}-templating`}
                    className="w-full max-md:h-11"
                    aria-invalid={serverTemplating ? true : undefined}
                    aria-describedby={`${ids}-templating-helper${serverTemplating ? ` ${ids}-templating-error` : ""}`}
                  >
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="none">{t("configs.sheet.templating.none")}</SelectItem>
                    <SelectItem value="golang">{t("configs.sheet.templating.golang")}</SelectItem>
                  </SelectContent>
                </Select>
                <FieldDescription id={`${ids}-templating-helper`}>
                  {t("configs.sheet.templating.helper")}
                </FieldDescription>
                {serverTemplating ? (
                  <FieldError id={`${ids}-templating-error`}>
                    {t("configs.errors.templating")}
                  </FieldError>
                ) : null}
              </Field>
            ) : null}
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
              {t(`${ns}.sheet.cancel`)}
            </Button>
            <Button
              type="button"
              className={`${TOUCH_HEIGHT} aria-disabled:cursor-not-allowed aria-disabled:opacity-50`}
              aria-disabled={pending}
              onClick={() => void submit()}
            >
              {pending ? <Spinner aria-hidden="true" /> : null}
              {t(`${ns}.sheet.submit`)}
            </Button>
          </SheetFooter>
        </SheetContent>
      </Sheet>

      <AlertDialog open={discarding} onOpenChange={setDiscarding}>
        <AlertDialogContent className="sm:max-w-120">
          <AlertDialogHeader>
            <AlertDialogTitle>{t(`${ns}.sheet.discard.title`)}</AlertDialogTitle>
            <AlertDialogDescription>{t(`${ns}.sheet.discard.body`)}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel autoFocus>{t(`${ns}.sheet.discard.keep`)}</AlertDialogCancel>
            <AlertDialogAction onClick={close}>
              {t(`${ns}.sheet.discard.discard`)}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  );
}
