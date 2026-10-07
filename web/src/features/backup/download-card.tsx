// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { zodResolver } from "@hookform/resolvers/zod";
import { useQuery } from "@tanstack/react-query";
import { Eye, EyeOff } from "lucide-react";
import { useEffect, useMemo, useRef, useState } from "react";
import { useForm } from "react-hook-form";
import { z } from "zod";

import {
  BackupExportError,
  backupInfoQuery,
  DEFAULT_MIN_PASSPHRASE,
  exportBackup,
  saveBlob,
} from "@/api/backup";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Field, FieldDescription, FieldError, FieldGroup, FieldLabel } from "@/components/ui/field";
import {
  InputGroup,
  InputGroupAddon,
  InputGroupButton,
  InputGroupInput,
} from "@/components/ui/input-group";
import { Spinner } from "@/components/ui/spinner";
import { SettingsCard } from "@/features/environments/settings-card";
import { useT } from "@/i18n/use-t";
import { toastSuccess } from "@/lib/toast";

// Validation here is UX only: the server checks the length again. Messages are
// i18n keys, never literals (D-65), and {min} is filled in where they are shown.
const KEY = {
  short: "backup.download.tooShort",
  mismatch: "backup.download.mismatch",
} as const;

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

// The server counts characters, so the check does too: "é" is one, however many
// bytes it takes.
const lengthOf = (value: string) => Array.from(value).length;

function schemaFor(min: number) {
  return z
    .object({
      passphrase: z.string().refine((v) => lengthOf(v) >= min, { error: KEY.short }),
      confirm: z.string(),
    })
    .refine((v) => v.passphrase === v.confirm, { path: ["confirm"], error: KEY.mismatch });
}

type Values = { passphrase: string; confirm: string };
type FieldName = keyof Values;

interface PasswordFieldProps {
  id: string;
  label: string;
  helper?: string;
  error?: string;
  showLabel: string;
  hideLabel: string;
  inputProps: object;
}

// PasswordField is a labelled password input with its own show/hide toggle. The
// control gets aria-invalid and an aria-describedby naming the helper and, while
// there is one, the error (UI-SPEC "Forms"). The browser is told this is a new
// password, so it neither fills one in nor offers to save it.
function PasswordField({
  id,
  label,
  helper,
  error,
  showLabel,
  hideLabel,
  inputProps,
}: PasswordFieldProps) {
  const [shown, setShown] = useState(false);
  const helperId = `${id}-helper`;
  const errorId = `${id}-error`;
  const describedBy = [helper ? helperId : null, error ? errorId : null].filter(Boolean).join(" ");
  return (
    <Field data-invalid={error ? true : undefined}>
      <FieldLabel htmlFor={id}>{label}</FieldLabel>
      <InputGroup className="h-11 md:h-8">
        <InputGroupInput
          id={id}
          {...inputProps}
          type={shown ? "text" : "password"}
          autoComplete="new-password"
          aria-invalid={Boolean(error)}
          aria-describedby={describedBy || undefined}
          className="h-full"
        />
        <InputGroupAddon align="inline-end">
          <InputGroupButton
            size="icon-xs"
            aria-label={shown ? hideLabel : showLabel}
            onClick={() => setShown((v) => !v)}
          >
            {shown ? <EyeOff aria-hidden="true" /> : <Eye aria-hidden="true" />}
          </InputGroupButton>
        </InputGroupAddon>
      </InputGroup>
      {helper ? <FieldDescription id={helperId}>{helper}</FieldDescription> : null}
      {error ? <FieldError id={errorId}>{error}</FieldError> : null}
    </Field>
  );
}

// DownloadCard asks for a passphrase twice and hands the encrypted backup the
// server makes to the browser (D-28, D-32). The passphrase lives in the form
// until the request is sent and is cleared once the file is saved; it is never
// put in an address, a toast or any state of its own.
export function DownloadCard() {
  const t = useT();
  const info = useQuery(backupInfoQuery);
  const min = info.data?.minPassphraseLength ?? DEFAULT_MIN_PASSPHRASE;
  const schema = useMemo(() => schemaFor(min), [min]);
  const [pending, setPending] = useState(false);
  const [failed, setFailed] = useState(false);
  const submitting = useRef(false);

  const form = useForm<Values>({
    resolver: zodResolver(schema),
    defaultValues: { passphrase: "", confirm: "" },
    mode: "onSubmit",
  });
  const errors = form.formState.errors;
  const errorOf = (name: FieldName) => {
    const message = errors[name]?.message;
    return typeof message === "string" ? t(message, { min }) : undefined;
  };

  // The fields are disabled while the backup is made, which drops focus. After a
  // failure it goes back to the passphrase, so the person can correct and retry.
  useEffect(() => {
    if (failed && !pending) form.setFocus("passphrase");
  }, [failed, pending, form]);

  async function submit(values: Values) {
    if (submitting.current) return;
    submitting.current = true;
    setPending(true);
    setFailed(false);
    try {
      const file = await exportBackup(values.passphrase);
      saveBlob(file.blob, file.name);
      form.reset();
      toastSuccess(t("backup.download.done"));
    } catch (error) {
      if (
        error instanceof BackupExportError &&
        error.status === 422 &&
        error.type === "passphrase_too_short"
      ) {
        form.setError("passphrase", { type: "server", message: KEY.short });
      } else {
        // 409 busy, 409 broken chain, 500 and an unreachable server read the same:
        // the reason is in the container logs, which the message points to.
        setFailed(true);
      }
    } finally {
      submitting.current = false;
      setPending(false);
    }
  }

  const passphrase = form.register("passphrase", { deps: ["confirm"] });
  const confirm = form.register("confirm");
  const show = t("backup.download.show");
  const hide = t("backup.download.hide");

  return (
    <SettingsCard title={t("backup.download.title")}>
      <form noValidate onSubmit={form.handleSubmit(submit)} className="flex flex-col gap-4">
        {failed ? (
          <Alert variant="destructive" role="alert">
            <AlertDescription>{t("backup.download.failed")}</AlertDescription>
          </Alert>
        ) : null}
        <fieldset disabled={pending} className="contents">
          <FieldGroup className="gap-4">
            <PasswordField
              id="backup-passphrase"
              label={t("backup.download.passphrase")}
              helper={t("backup.download.passphraseHelper", { min })}
              error={errorOf("passphrase")}
              showLabel={show}
              hideLabel={hide}
              inputProps={passphrase}
            />
            <PasswordField
              id="backup-confirm"
              label={t("backup.download.confirm")}
              error={errorOf("confirm")}
              showLabel={show}
              hideLabel={hide}
              inputProps={confirm}
            />
          </FieldGroup>
          <div>
            <Button type="submit" className={`${TOUCH_HEIGHT} max-md:w-full`}>
              {pending ? (
                <>
                  <Spinner aria-hidden="true" />
                  {t("backup.download.pending")}
                </>
              ) : (
                t("backup.download.submit")
              )}
            </Button>
          </div>
        </fieldset>
      </form>
    </SettingsCard>
  );
}
