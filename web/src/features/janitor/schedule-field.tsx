// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useState } from "react";

import { type SchedulePreset, useSchedulePreview } from "@/api/janitor";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { type ScheduleDraft, scheduleChange } from "@/features/janitor/policy-diff";
import { formatServerTime } from "@/features/janitor/status-text";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

export const PRESETS: readonly SchedulePreset[] = [
  "hourly",
  "every_6h",
  "daily",
  "weekly",
  "custom",
];

// The weekdays as the Weekly preset lists them, Monday first; the value is the cron day of week.
export const WEEKDAYS = [1, 2, 3, 4, 5, 6, 0] as const;

// What the server said is wrong with the schedule: it is not an expression the janitor can read, it
// fires more often than every 15 minutes, or the time of day is missing.
export type ScheduleProblem = "invalid" | "too_frequent" | "time_missing";

const TOUCH = "max-md:h-11";

interface ScheduleFieldProps {
  // The environment whose schedule-preview route is asked for the next runs of a custom expression.
  // Absent when the person may read no environment: a custom expression is then checked on save.
  envId?: string;
  // The id the controls are named from; the preset select is `${id}-preset`.
  id: string;
  value: ScheduleDraft;
  onChange: (next: ScheduleDraft) => void;
  // The server's time zone, which every time on this row is read in.
  timezone: string;
  // Why the person may not change the schedule (NeedsTooltip); absent when they may.
  locked?: string;
  // A refusal the card already knows of (a blank expression, or the server's answer to a save). It
  // takes the place of the live line.
  problem?: ScheduleProblem | null;
}

// ScheduleField is the Schedule row's control: a preset (Hourly, Every 6 hours, Daily, Weekly) or a
// cron expression of one's own. The inputs that belong to the preset appear beside it, and under a
// custom expression the server says when it would next run. The times are the scheduler's own, in
// the server's time zone: the browser does no cron arithmetic (D-02).
export function ScheduleField({
  envId,
  id,
  value,
  onChange,
  timezone,
  locked,
  problem,
}: ScheduleFieldProps) {
  const t = useT();
  const custom = value.preset === "custom";
  const preview = useSchedulePreview(envId ?? "", value.cron, custom && envId !== undefined);
  // A select that may not be changed is held shut rather than disabled, so it keeps its place in the
  // tab order and its reason opens on focus (NeedsTooltip). Radix opens it on pointer down, which
  // the tooltip's click guard does not stop.
  const [presetOpen, setPresetOpen] = useState(false);
  const [dayOpen, setDayOpen] = useState(false);
  const readOnly = locked !== undefined;

  const set = (patch: Partial<ScheduleDraft>) => onChange(scheduleChange(value, patch));

  const lineProblem: ScheduleProblem | null =
    problem ??
    (custom && preview.data && !preview.data.valid
      ? preview.data.problem === "too_frequent"
        ? "too_frequent"
        : "invalid"
      : null);
  const nextRuns =
    custom && preview.data?.valid && (preview.data.nextRuns?.length ?? 0) > 0
      ? t("janitorPolicy.schedule.nextRuns", {
          times: (preview.data.nextRuns ?? [])
            .map((at) => formatServerTime(at, preview.data?.timezone ?? timezone))
            .join("; "),
        })
      : "";
  const lineText = lineProblem
    ? t(
        lineProblem === "too_frequent"
          ? "janitorPolicy.schedule.tooFrequent"
          : lineProblem === "time_missing"
            ? "janitorPolicy.schedule.timeMissing"
            : "janitorPolicy.schedule.invalid",
      )
    : nextRuns || (custom && preview.waiting ? t("janitorPolicy.schedule.checking") : "");

  const timeInput = (
    <div className="flex flex-col gap-1.5">
      <Label htmlFor={`${id}-time`}>{t("janitorPolicy.schedule.at")}</Label>
      <NeedsTooltip reason={locked}>
        <Input
          id={`${id}-time`}
          type="time"
          value={value.time}
          readOnly={readOnly}
          aria-invalid={problem === "time_missing" || undefined}
          className={cn("w-32", TOUCH)}
          onChange={(e) => set({ time: e.target.value })}
        />
      </NeedsTooltip>
    </div>
  );

  return (
    <div className="flex flex-col gap-3">
      <div className="flex flex-wrap items-end gap-3">
        <div className="flex flex-col gap-1.5">
          <Label htmlFor={`${id}-preset`}>{t("janitorPolicy.schedule.run")}</Label>
          <Select
            value={value.preset}
            open={presetOpen && !readOnly}
            onOpenChange={setPresetOpen}
            onValueChange={(preset) => set({ preset: preset as SchedulePreset })}
          >
            <NeedsTooltip reason={locked}>
              <SelectTrigger id={`${id}-preset`} className={cn("w-48", TOUCH)}>
                <SelectValue />
              </SelectTrigger>
            </NeedsTooltip>
            <SelectContent>
              {PRESETS.map((preset) => (
                <SelectItem key={preset} value={preset}>
                  {t(`janitorPolicy.schedule.presets.${preset}`)}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
        {/* The inputs the preset needs appear and go as it changes; a screen reader hears them. */}
        <div aria-live="polite" className="flex flex-wrap items-end gap-3">
          {value.preset === "weekly" ? (
            <div className="flex flex-col gap-1.5">
              <Label htmlFor={`${id}-day`}>{t("janitorPolicy.schedule.on")}</Label>
              <Select
                value={String(value.weekday)}
                open={dayOpen && !readOnly}
                onOpenChange={setDayOpen}
                onValueChange={(day) => set({ weekday: Number(day) })}
              >
                <NeedsTooltip reason={locked}>
                  <SelectTrigger id={`${id}-day`} className={cn("w-40", TOUCH)}>
                    <SelectValue />
                  </SelectTrigger>
                </NeedsTooltip>
                <SelectContent>
                  {WEEKDAYS.map((day) => (
                    <SelectItem key={day} value={String(day)}>
                      {t(`janitorPolicy.schedule.weekdays.${day}`)}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          ) : null}
          {value.preset === "daily" || value.preset === "weekly" ? timeInput : null}
          {custom ? (
            <div className="flex min-w-48 flex-1 flex-col gap-1.5 md:max-w-80">
              <Label htmlFor={`${id}-cron`}>{t("janitorPolicy.schedule.cron")}</Label>
              <NeedsTooltip reason={locked}>
                <Input
                  id={`${id}-cron`}
                  value={value.cron}
                  readOnly={readOnly}
                  spellCheck={false}
                  autoComplete="off"
                  autoCapitalize="none"
                  placeholder={t("janitorPolicy.schedule.cronPlaceholder")}
                  aria-invalid={
                    lineProblem === "invalid" || lineProblem === "too_frequent" || undefined
                  }
                  aria-describedby={`${id}-line`}
                  className={cn("font-mono", TOUCH)}
                  onChange={(e) => set({ cron: e.target.value })}
                />
              </NeedsTooltip>
            </div>
          ) : null}
        </div>
      </div>
      <p className="text-sm text-muted-foreground">
        {t("janitorPolicy.schedule.helper", { timezone })}
      </p>
      {custom || lineProblem ? (
        <p
          id={`${id}-line`}
          role="status"
          className={cn("text-sm", lineProblem ? "text-destructive" : "text-muted-foreground")}
        >
          {lineText}
        </p>
      ) : null}
    </div>
  );
}
