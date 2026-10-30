// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useQuery } from "@tanstack/react-query";
import { type FormEvent, useId, useState } from "react";

import {
  FAILURES_MAX,
  FAILURES_MIN,
  restartLoopSettingsQuery,
  useSaveRestartLoopSettings,
  WINDOW_MAX,
  WINDOW_MIN,
} from "@/api/restart-loops";
import { Button } from "@/components/ui/button";
import { Field, FieldDescription, FieldError, FieldGroup, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { SettingsCard } from "@/features/environments/settings-card";
import { usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { toastSuccess } from "@/lib/toast";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

interface RestartLoopCardProps {
  envId: string;
}

// wholeNumber reads what was typed as a whole number in a range, or undefined. Only digits count:
// "3.5", "1e1" and "" are no number a person means to set.
function wholeNumber(text: string, min: number, max: number): number | undefined {
  const trimmed = text.trim();
  if (!/^\d{1,4}$/.test(trimmed)) return undefined;
  const n = Number(trimmed);
  return n >= min && n <= max ? n : undefined;
}

// RestartLoopCard is "Restart-loop detection" in Settings > Environments > {env} (RES-13, D-14, Q3,
// Q5): how many failed tasks inside how many minutes make a restart loop here. It says how many
// failed tasks Docker itself keeps per slot, because a threshold above that is only met by loops
// that Coaming's own earlier checks have seen. It is for someone holding environments.configure on
// the environment; without it nothing is drawn and nothing is read.
export function RestartLoopCard({ envId }: RestartLoopCardProps) {
  const allowed = usePermission("environments.configure", envId);
  return allowed ? <RestartLoopBody envId={envId} /> : null;
}

function RestartLoopBody({ envId }: RestartLoopCardProps) {
  const t = useT();
  const id = useId();
  const settings = useQuery(restartLoopSettingsQuery(envId));
  const save = useSaveRestartLoopSettings(envId);
  // What was typed, until the person saves: a field not touched shows the environment's value.
  const [typed, setTyped] = useState<{ failures?: string; window?: string }>({});
  const [errors, setErrors] = useState<{ failures?: boolean; window?: boolean; save?: boolean }>(
    {},
  );
  const failures = typed.failures ?? String(settings.data?.failures ?? "");
  const window = typed.window ?? String(settings.data?.windowMinutes ?? "");
  const limit = settings.data?.historyLimit ?? 0;

  function submit(event: FormEvent) {
    event.preventDefault();
    const f = wholeNumber(failures, FAILURES_MIN, FAILURES_MAX);
    const w = wholeNumber(window, WINDOW_MIN, WINDOW_MAX);
    const next = { failures: f === undefined, window: w === undefined };
    setErrors(next);
    if (f === undefined || w === undefined) return;
    save.mutate(
      { failures: f, windowMinutes: w },
      {
        onSuccess: () => {
          setTyped({});
          toastSuccess(t("restartLoop.settings.saved"));
        },
        onError: (error) => {
          setErrors({
            failures: error.type === "failures_invalid",
            window: error.type === "window_invalid",
            save: error.type !== "failures_invalid" && error.type !== "window_invalid",
          });
        },
      },
    );
  }

  return (
    <SettingsCard title={t("restartLoop.settings.title")}>
      <p className="text-sm text-muted-foreground">{t("restartLoop.settings.caption")}</p>
      {settings.isPending ? (
        <div className="flex flex-col gap-3" aria-busy="true">
          <Skeleton className="h-8 w-40" />
          <Skeleton className="h-8 w-40" />
        </div>
      ) : settings.isError ? (
        <p role="alert" className="text-sm text-destructive">
          {t("restartLoop.settings.loadFailed")}
        </p>
      ) : (
        <form onSubmit={submit} noValidate className="flex flex-col gap-4">
          <fieldset disabled={save.isPending} className="contents">
            <FieldGroup className="gap-4">
              <Field data-invalid={errors.failures ? true : undefined}>
                <FieldLabel htmlFor={`${id}-failures`}>
                  {t("restartLoop.settings.failures")}
                </FieldLabel>
                <Input
                  id={`${id}-failures`}
                  inputMode="numeric"
                  autoComplete="off"
                  spellCheck={false}
                  value={failures}
                  aria-invalid={Boolean(errors.failures)}
                  aria-describedby={`${id}-failures-help`}
                  className={`${TOUCH_HEIGHT} md:max-w-40`}
                  onChange={(e) => {
                    setTyped((prev) => ({ ...prev, failures: e.target.value }));
                    setErrors((prev) => ({ ...prev, failures: false, save: false }));
                  }}
                />
                <FieldDescription id={`${id}-failures-help`}>
                  {t("restartLoop.settings.failuresHelper")}
                </FieldDescription>
                {limit > 0 ? (
                  <FieldDescription>
                    {t("restartLoop.settings.retention", { kept: limit - 1 })}
                  </FieldDescription>
                ) : null}
                {errors.failures ? (
                  <FieldError role="alert">{t("restartLoop.settings.failuresInvalid")}</FieldError>
                ) : null}
              </Field>
              <Field data-invalid={errors.window ? true : undefined}>
                <FieldLabel htmlFor={`${id}-window`}>{t("restartLoop.settings.window")}</FieldLabel>
                <Input
                  id={`${id}-window`}
                  inputMode="numeric"
                  autoComplete="off"
                  spellCheck={false}
                  value={window}
                  aria-invalid={Boolean(errors.window)}
                  aria-describedby={`${id}-window-help`}
                  className={`${TOUCH_HEIGHT} md:max-w-40`}
                  onChange={(e) => {
                    setTyped((prev) => ({ ...prev, window: e.target.value }));
                    setErrors((prev) => ({ ...prev, window: false, save: false }));
                  }}
                />
                <FieldDescription id={`${id}-window-help`}>
                  {t("restartLoop.settings.windowHelper")}
                </FieldDescription>
                {errors.window ? (
                  <FieldError role="alert">{t("restartLoop.settings.windowInvalid")}</FieldError>
                ) : null}
              </Field>
            </FieldGroup>
          </fieldset>
          {errors.save ? (
            <p role="alert" className="text-sm text-destructive">
              {t("restartLoop.settings.saveFailed")}
            </p>
          ) : null}
          <div>
            <Button
              type="submit"
              className={`${TOUCH_HEIGHT} max-md:w-full`}
              aria-disabled={save.isPending || undefined}
            >
              {save.isPending ? (
                <>
                  <Spinner aria-hidden="true" />
                  {t("restartLoop.settings.saving")}
                </>
              ) : (
                t("restartLoop.settings.save")
              )}
            </Button>
          </div>
        </form>
      )}
    </SettingsCard>
  );
}
