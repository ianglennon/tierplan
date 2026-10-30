// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { zodResolver } from "@hookform/resolvers/zod";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Lock } from "lucide-react";
import { useMemo, useState } from "react";
import { Controller, useForm } from "react-hook-form";
import {
  type BackupSchedule,
  backupInfoQuery,
  DEFAULT_MIN_PASSPHRASE,
  SCHEDULE_KEY,
  ScheduleSaveError,
  saveSchedule,
  scheduleQuery,
} from "@/api/backup";
import { RelativeTime } from "@/components/relative-time";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Field, FieldDescription, FieldError, FieldGroup, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { InputGroup, InputGroupAddon, InputGroupInput } from "@/components/ui/input-group";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { Switch } from "@/components/ui/switch";
import { PasswordField } from "@/features/backup/password-field";
import { SavedBackupsTable } from "@/features/backup/saved-backups-table";
import { SettingsCard } from "@/features/environments/settings-card";
import { useT } from "@/i18n/use-t";
import { formatAbsolute, formatBytes } from "@/lib/format";
import { toastSuccess } from "@/lib/toast";
import { z } from "@/lib/zod-config";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

// Marks where a component goes inside a translated sentence.
const SLOT = "\u0001";

// Validation here is UX only: the server checks every field again. Messages are
// i18n keys, never literals (D-65).
const KEY = {
  time: "schedule.time.invalid",
  keep: "schedule.keep.invalid",
  short: "schedule.passphrase.tooShort",
  mismatch: "schedule.passphrase.mismatch",
  required: "schedule.passphrase.required",
} as const;

// Sunday first, as the server counts the days.
const WEEKDAYS = ["0", "1", "2", "3", "4", "5", "6"] as const;

const TIME = /^([01]\d|2[0-3]):[0-5]\d$/;

// The server counts characters, so the check does too.
const lengthOf = (value: string) => Array.from(value).length;

function schemaFor(min: number, passphraseSaved: boolean) {
  return z
    .object({
      enabled: z.boolean(),
      frequency: z.enum(["daily", "weekly"]),
      weekday: z.string().regex(/^[0-6]$/),
      timeUtc: z.string().regex(TIME, { error: KEY.time }),
      keep: z
        .string()
        .refine((v) => /^\d+$/.test(v) && Number(v) >= 1 && Number(v) <= 365, { error: KEY.keep }),
      passphrase: z.string(),
      confirm: z.string(),
    })
    .superRefine((v, ctx) => {
      if (v.passphrase !== "" && lengthOf(v.passphrase) < min) {
        ctx.addIssue({ code: "custom", path: ["passphrase"], message: KEY.short });
      }
      if (v.passphrase === "" && v.enabled && !passphraseSaved) {
        ctx.addIssue({ code: "custom", path: ["passphrase"], message: KEY.required });
      }
      if (v.confirm !== v.passphrase) {
        ctx.addIssue({ code: "custom", path: ["confirm"], message: KEY.mismatch });
      }
    });
}

type Values = {
  enabled: boolean;
  frequency: BackupSchedule["frequency"];
  weekday: string;
  timeUtc: string;
  keep: string;
  passphrase: string;
  confirm: string;
};

// valuesOf is what the form shows for a saved schedule: the passphrase fields are
// always empty, because the passphrase is never read back.
function valuesOf(s: BackupSchedule): Values {
  return {
    enabled: s.enabled,
    frequency: s.frequency,
    weekday: String(s.weekday),
    timeUtc: s.timeUtc,
    keep: String(s.keep),
    passphrase: "",
    confirm: "",
  };
}

// SchedulePanel is the "Scheduled backups" card of Settings > Backup (D-32): the
// switch, when and how many, where the files go, the passphrase that locks them,
// the status of the last run and the backups saved so far.
export function SchedulePanel() {
  const t = useT();
  const schedule = useQuery(scheduleQuery);
  return (
    <SettingsCard title={t("schedule.title")}>
      {schedule.isPending ? (
        <div className="flex flex-col gap-4" aria-hidden="true">
          <Skeleton className="h-8 w-64" />
          <Skeleton className="h-8 w-full" />
          <Skeleton className="h-8 w-full" />
        </div>
      ) : null}
      {schedule.isError ? (
        <Alert variant="destructive">
          <AlertDescription>{t("common.errors.unreachable")}</AlertDescription>
        </Alert>
      ) : null}
      {schedule.data ? <ScheduleForm schedule={schedule.data} /> : null}
      <SavedBackupsTable />
    </SettingsCard>
  );
}

// Sentence puts a component into a translated sentence where SLOT is.
function Sentence({ text, slot }: { text: string; slot: React.ReactNode }) {
  const [before = "", after = ""] = text.split(SLOT);
  return (
    <>
      {before}
      {slot}
      {after}
    </>
  );
}

function ScheduleForm({ schedule }: { schedule: BackupSchedule }) {
  const t = useT();
  const queryClient = useQueryClient();
  const info = useQuery(backupInfoQuery);
  const min = info.data?.minPassphraseLength ?? DEFAULT_MIN_PASSPHRASE;
  const schema = useMemo(() => schemaFor(min, schedule.passphraseSaved), [min, schedule]);

  const form = useForm<Values>({
    resolver: zodResolver(schema),
    defaultValues: valuesOf(schedule),
    mode: "onSubmit",
  });
  const errors = form.formState.errors;
  const errorOf = (name: keyof Values) => {
    const message = errors[name]?.message;
    return typeof message === "string" ? t(message, { min }) : undefined;
  };

  // A save the server did not take: the message stays until the next try or Cancel.
  const [failed, setFailed] = useState(false);

  const save = useMutation({
    mutationFn: (v: Values) =>
      saveSchedule({
        enabled: v.enabled,
        frequency: v.frequency,
        weekday: Number(v.weekday),
        timeUtc: v.timeUtc,
        keep: Number(v.keep),
        passphrase: v.passphrase,
      }),
    onMutate: () => setFailed(false),
    onSuccess: (saved) => {
      queryClient.setQueryData(SCHEDULE_KEY, saved);
      // The passphrase leaves the form as soon as it is saved.
      form.reset(valuesOf(saved));
      toastSuccess(t("schedule.done"));
    },
    onError: (error) => {
      if (error instanceof ScheduleSaveError && error.type === "passphrase_too_short") {
        form.setError("passphrase", { type: "server", message: KEY.short });
      } else if (error instanceof ScheduleSaveError && error.type === "passphrase_required") {
        form.setError("passphrase", { type: "server", message: KEY.required });
      } else {
        setFailed(true);
      }
    },
  });

  const passphrase = form.register("passphrase", { deps: ["confirm"] });
  const confirm = form.register("confirm");
  const show = t("schedule.passphrase.show");
  const hide = t("schedule.passphrase.hide");
  const pending = save.isPending;
  const frequency = form.watch("frequency");
  const dirty = form.formState.isDirty;

  const last = schedule.lastSuccess;

  return (
    <form
      noValidate
      onSubmit={form.handleSubmit((v) => save.mutate(v))}
      className="flex flex-col gap-4"
    >
      <fieldset disabled={pending} className="contents">
        <FieldGroup className="gap-4">
          <div className="flex items-center gap-3">
            <Controller
              control={form.control}
              name="enabled"
              render={({ field }) => (
                <Switch
                  id="schedule-enabled"
                  checked={field.value}
                  onCheckedChange={field.onChange}
                />
              )}
            />
            <Label htmlFor="schedule-enabled">{t("schedule.enabled")}</Label>
          </div>

          <Field>
            <FieldLabel htmlFor="schedule-frequency">{t("schedule.frequency.label")}</FieldLabel>
            <Controller
              control={form.control}
              name="frequency"
              render={({ field }) => (
                <Select value={field.value} onValueChange={field.onChange}>
                  <SelectTrigger id="schedule-frequency" className={`${TOUCH_HEIGHT} w-full`}>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="daily">{t("schedule.frequency.daily")}</SelectItem>
                    <SelectItem value="weekly">{t("schedule.frequency.weekly")}</SelectItem>
                  </SelectContent>
                </Select>
              )}
            />
          </Field>

          {frequency === "weekly" ? (
            <Field>
              <FieldLabel htmlFor="schedule-weekday">{t("schedule.weekday.label")}</FieldLabel>
              <Controller
                control={form.control}
                name="weekday"
                render={({ field }) => (
                  <Select value={field.value} onValueChange={field.onChange}>
                    <SelectTrigger id="schedule-weekday" className={`${TOUCH_HEIGHT} w-full`}>
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      {WEEKDAYS.map((day) => (
                        <SelectItem key={day} value={day}>
                          {t(`schedule.weekday.day.${day}`)}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                )}
              />
            </Field>
          ) : null}

          <Field data-invalid={errors.timeUtc ? true : undefined}>
            <FieldLabel htmlFor="schedule-time">{t("schedule.time.label")}</FieldLabel>
            <Input
              id="schedule-time"
              inputMode="numeric"
              autoComplete="off"
              placeholder="03:00"
              maxLength={5}
              aria-invalid={Boolean(errors.timeUtc)}
              aria-describedby={errors.timeUtc ? "schedule-time-error" : undefined}
              className={`${TOUCH_HEIGHT} w-28 font-mono`}
              {...form.register("timeUtc")}
            />
            {errors.timeUtc ? (
              <FieldError id="schedule-time-error">{errorOf("timeUtc")}</FieldError>
            ) : null}
          </Field>

          <Field data-invalid={errors.keep ? true : undefined}>
            <FieldLabel htmlFor="schedule-keep">{t("schedule.keep.label")}</FieldLabel>
            <Input
              id="schedule-keep"
              type="number"
              inputMode="numeric"
              min={1}
              max={365}
              step={1}
              aria-invalid={Boolean(errors.keep)}
              aria-describedby="schedule-keep-helper"
              className={`${TOUCH_HEIGHT} w-28`}
              {...form.register("keep")}
            />
            <FieldDescription id="schedule-keep-helper">
              {t("schedule.keep.helper")}
            </FieldDescription>
            {errors.keep ? <FieldError>{errorOf("keep")}</FieldError> : null}
          </Field>

          <Field>
            <FieldLabel htmlFor="schedule-location">{t("schedule.location.label")}</FieldLabel>
            <InputGroup className={TOUCH_HEIGHT}>
              <InputGroupAddon align="inline-start">
                <Lock aria-hidden="true" />
              </InputGroupAddon>
              <InputGroupInput
                id="schedule-location"
                readOnly
                value={schedule.dir}
                aria-describedby="schedule-location-helper"
                className="h-full font-mono text-xs"
              />
            </InputGroup>
            <FieldDescription id="schedule-location-helper">
              {t("schedule.location.helper")}
            </FieldDescription>
          </Field>

          <PasswordField
            id="schedule-passphrase"
            label={t("schedule.passphrase.label")}
            helper={t("schedule.passphrase.helper")}
            error={errorOf("passphrase")}
            showLabel={show}
            hideLabel={hide}
            inputProps={{
              ...passphrase,
              placeholder: schedule.passphraseSaved ? t("schedule.passphrase.saved") : undefined,
            }}
          />
          <PasswordField
            id="schedule-confirm"
            label={t("schedule.passphrase.confirm")}
            error={errorOf("confirm")}
            showLabel={show}
            hideLabel={hide}
            inputProps={confirm}
          />
        </FieldGroup>

        <div className="flex flex-col gap-1 text-sm text-muted-foreground">
          {schedule.lastRun?.ok === false ? (
            <p className="text-destructive">
              <Sentence
                text={t("schedule.status.failed", {
                  time: SLOT,
                  reason: (schedule.lastRun.error ?? "").replace(/\.+$/, ""),
                })}
                slot={<RelativeTime value={schedule.lastRun.at} />}
              />
            </p>
          ) : null}
          {last ? (
            <p>
              <Sentence
                text={t("schedule.status.last", {
                  time: SLOT,
                  size: formatBytes(last.size ?? 0),
                })}
                slot={<RelativeTime value={last.at} />}
              />
            </p>
          ) : null}
          {!schedule.lastRun && !last ? <p>{t("schedule.status.none")}</p> : null}
          {schedule.enabled && schedule.nextRun ? (
            <p>{t("schedule.status.next", { time: formatAbsolute(schedule.nextRun) })}</p>
          ) : null}
        </div>

        {failed ? (
          <Alert variant="destructive" role="alert">
            <AlertDescription>{t("schedule.failed")}</AlertDescription>
          </Alert>
        ) : null}

        <div className="flex flex-wrap gap-2">
          <Button type="submit" className={`${TOUCH_HEIGHT} max-md:w-full`}>
            {pending ? (
              <>
                <Spinner aria-hidden="true" />
                {t("schedule.saving")}
              </>
            ) : (
              t("schedule.save")
            )}
          </Button>
          {dirty || failed ? (
            <Button
              type="button"
              variant="outline"
              className={`${TOUCH_HEIGHT} max-md:w-full`}
              onClick={() => {
                setFailed(false);
                form.reset(valuesOf(schedule));
              }}
            >
              {t("schedule.cancel")}
            </Button>
          ) : null}
        </div>
      </fieldset>
    </form>
  );
}
