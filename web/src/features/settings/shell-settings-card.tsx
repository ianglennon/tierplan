// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { zodResolver } from "@hookform/resolvers/zod";
import { useState } from "react";
import { useForm } from "react-hook-form";
import { type ShellSettings, useShellSettings, useUpdateShellSettings } from "@/api/shells";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Field, FieldDescription, FieldError, FieldGroup, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { toastSuccess } from "@/lib/toast";
import { z } from "@/lib/zod-config";

// The ranges the server enforces again (D-29, D-31). Messages are i18n keys, never
// literals (D-65).
const IDLE = { min: 5, max: 240, error: "settings.shells.idle.error" } as const;
const CAP = { min: 1, max: 50, error: "settings.shells.cap.error" } as const;

// A whole number in the range, typed into a number field (so a string), read as a number.
function wholeNumber(r: { min: number; max: number; error: string }) {
  return z
    .string()
    .trim()
    .regex(/^\d+$/, { error: r.error })
    .transform(Number)
    .pipe(z.number().min(r.min, { error: r.error }).max(r.max, { error: r.error }));
}

const schema = z.object({ idleMinutes: wholeNumber(IDLE), maxPerUser: wholeNumber(CAP) });
type FormInput = z.input<typeof schema>;
type Values = z.output<typeof schema>;

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

function ShellSettingsForm({ settings }: { settings: ShellSettings }) {
  const t = useT();
  const canWrite = usePermission("settings.write");
  const update = useUpdateShellSettings();
  const [formError, setFormError] = useState<string | null>(null);

  const form = useForm<FormInput, unknown, Values>({
    resolver: zodResolver(schema),
    defaultValues: {
      idleMinutes: String(settings.idleMinutes),
      maxPerUser: String(settings.maxPerUser),
    },
    mode: "onChange",
  });
  const errors = form.formState.errors;
  const errorOf = (name: keyof FormInput) => {
    const message = errors[name]?.message;
    return typeof message === "string" ? t(message) : undefined;
  };

  // Save waits for a change that is valid in both fields: what is typed, read as a
  // number, against what the server holds.
  const typed = schema.safeParse(form.watch());
  const dirty =
    typed.success &&
    (typed.data.idleMinutes !== settings.idleMinutes ||
      typed.data.maxPerUser !== settings.maxPerUser);

  async function save(values: Values) {
    if (!canWrite || update.isPending) return;
    setFormError(null);
    // Only the limit that changed is sent, so the audit entry names what was touched.
    const body: Partial<ShellSettings> = {};
    if (values.idleMinutes !== settings.idleMinutes) body.idleMinutes = values.idleMinutes;
    if (values.maxPerUser !== settings.maxPerUser) body.maxPerUser = values.maxPerUser;
    try {
      const saved = await update.mutateAsync(body);
      form.reset({ idleMinutes: String(saved.idleMinutes), maxPerUser: String(saved.maxPerUser) });
      toastSuccess(t("settings.saved"));
    } catch (e) {
      const problem = (e as { problem?: { status: number; fields: { location: string }[] } })
        .problem;
      if (problem?.status === 422) {
        let first: keyof FormInput | null = null;
        for (const f of problem.fields) {
          const name =
            f.location === "body.idleMinutes"
              ? "idleMinutes"
              : f.location === "body.maxPerUser"
                ? "maxPerUser"
                : null;
          if (!name) continue;
          form.setError(name, {
            type: "server",
            message: name === "idleMinutes" ? IDLE.error : CAP.error,
          });
          first ??= name;
        }
        if (first) {
          form.setFocus(first);
          return;
        }
      }
      setFormError(problem ? "common.mutationFailed" : "common.errors.unreachable");
    }
  }

  const readOnly = !canWrite;
  const pending = update.isPending;
  const idleError = errorOf("idleMinutes");
  const capError = errorOf("maxPerUser");
  const idle = form.register("idleMinutes");
  const cap = form.register("maxPerUser");

  return (
    <form noValidate onSubmit={form.handleSubmit(save)} className="flex flex-col gap-4">
      {formError ? (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{t(formError)}</AlertDescription>
        </Alert>
      ) : null}
      {readOnly ? (
        <p className="text-xs text-muted-foreground">{t("settings.general.readOnly")}</p>
      ) : null}
      <fieldset disabled={pending} className="contents">
        <FieldGroup className="gap-4">
          <Field data-invalid={idleError ? true : undefined}>
            <FieldLabel htmlFor="shell-idle-minutes">{t("settings.shells.idle.label")}</FieldLabel>
            <Input
              id="shell-idle-minutes"
              type="number"
              inputMode="numeric"
              min={IDLE.min}
              max={IDLE.max}
              step={1}
              className={TOUCH_HEIGHT}
              readOnly={readOnly}
              aria-invalid={Boolean(idleError)}
              aria-describedby={[
                "shell-idle-minutes-helper",
                idleError ? "shell-idle-minutes-error" : null,
              ]
                .filter(Boolean)
                .join(" ")}
              {...idle}
            />
            <FieldDescription id="shell-idle-minutes-helper">
              {t("settings.shells.idle.helper")}
            </FieldDescription>
            {idleError ? <FieldError id="shell-idle-minutes-error">{idleError}</FieldError> : null}
          </Field>
          <Field data-invalid={capError ? true : undefined}>
            <FieldLabel htmlFor="shell-max-per-user">{t("settings.shells.cap.label")}</FieldLabel>
            <Input
              id="shell-max-per-user"
              type="number"
              inputMode="numeric"
              min={CAP.min}
              max={CAP.max}
              step={1}
              className={TOUCH_HEIGHT}
              readOnly={readOnly}
              aria-invalid={Boolean(capError)}
              aria-describedby={[
                "shell-max-per-user-helper",
                capError ? "shell-max-per-user-error" : null,
              ]
                .filter(Boolean)
                .join(" ")}
              {...cap}
            />
            <FieldDescription id="shell-max-per-user-helper">
              {t("settings.shells.cap.helper")}
            </FieldDescription>
            {capError ? <FieldError id="shell-max-per-user-error">{capError}</FieldError> : null}
          </Field>
          {readOnly ? null : (
            <div>
              <Button type="submit" className={TOUCH_HEIGHT} disabled={!dirty || pending}>
                {pending ? (
                  <>
                    <Spinner aria-hidden="true" />
                    {t("settings.general.pending")}
                  </>
                ) : (
                  t("settings.general.submit")
                )}
              </Button>
            </div>
          )}
        </FieldGroup>
      </fieldset>
    </form>
  );
}

// ShellSettingsCard is the "Container shells" card of Settings > General: how long an
// idle browser shell lives and how many one person may hold (D-29, D-31). Reading needs
// settings.read and changing settings.write; the server checks both again. The fields
// are never blank: they hold the current values, which are the defaults until an admin
// changes them.
export function ShellSettingsCard() {
  const t = useT();
  const settings = useShellSettings();

  return (
    <Card className="max-w-xl">
      <CardHeader>
        <CardTitle>
          <h2>{t("settings.shells.title")}</h2>
        </CardTitle>
        <CardDescription>{t("settings.shells.caption")}</CardDescription>
      </CardHeader>
      <CardContent>
        {settings.isPending ? (
          <div className="flex flex-col gap-3" aria-busy="true">
            <span className="sr-only" role="status">
              {t("settings.shells.loading")}
            </span>
            <Skeleton className="h-16 w-full" />
            <Skeleton className="h-16 w-full" />
            <Button type="button" className={TOUCH_HEIGHT} disabled>
              {t("settings.general.submit")}
            </Button>
          </div>
        ) : settings.isError ? (
          <Alert variant="destructive">
            <AlertDescription className="flex flex-wrap items-center gap-3">
              <span>{t("settings.shells.loadFailed")}</span>
              <Button
                type="button"
                variant="outline"
                size="sm"
                disabled={settings.isFetching}
                onClick={() => void settings.refetch()}
              >
                {t("settings.general.retry")}
              </Button>
            </AlertDescription>
          </Alert>
        ) : (
          <ShellSettingsForm
            key={`${settings.data.idleMinutes}\u0000${settings.data.maxPerUser}`}
            settings={settings.data}
          />
        )}
      </CardContent>
    </Card>
  );
}
