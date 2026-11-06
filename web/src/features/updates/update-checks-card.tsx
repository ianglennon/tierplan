// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useEffect, useId, useState } from "react";
import { Trans } from "react-i18next";

import type { EnvironmentItem } from "@/api/environments";
import {
  conflictOf,
  type UpdateCheckStatus,
  UpdateSettingsError,
  useSaveUpdateCheckPolicy,
  useUpdateCheckDefaults,
  useUpdateCheckStatus,
} from "@/api/updates";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { RelativeTime } from "@/components/relative-time";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { Switch } from "@/components/ui/switch";
import { SettingsCard } from "@/features/environments/settings-card";
import { cronOf } from "@/features/janitor/policy-diff";
import { PolicyRow, rowIds } from "@/features/janitor/policy-row";
import { ScheduleField, type ScheduleProblem } from "@/features/janitor/schedule-field";
import { formatServerTime } from "@/features/janitor/status-text";
import {
  type Draft,
  type FieldName,
  fieldsOf,
  isDirty,
  viewOf,
  withEdit,
} from "@/features/updates/update-checks-form";
import { usePermission } from "@/hooks/use-me";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { toastSuccess } from "@/lib/toast";

type Failure =
  | { kind: "conflict"; by: string }
  | { kind: "schedule"; problem: ScheduleProblem }
  | { kind: "failed" };

interface UpdateChecksCardProps {
  env: EnvironmentItem;
}

// StatusLine is "Last checked {RelativeTime}. Next check {absolute time}." or "Not checked yet.". The
// next check is shown in the server's time zone, the one the schedule is read in (D-02).
function StatusLine({ status }: { status: UpdateCheckStatus }) {
  const next = status.nextCheckAt ? formatServerTime(status.nextCheckAt, status.timezone) : "";
  const last = status.lastCheckedAt !== "";
  const key = last
    ? next
      ? "updates.checks.status.both"
      : "updates.checks.status.last"
    : next
      ? "updates.checks.status.nextOnly"
      : "updates.checks.status.never";
  return (
    <p className="text-sm text-muted-foreground">
      <Trans
        i18nKey={key}
        ns="updates"
        values={{ next }}
        components={{ time: <RelativeTime value={status.lastCheckedAt} /> }}
      />
    </p>
  );
}

// UpdateChecksCard is the "Image update checks" card of an environment's settings page (anchor
// #update-checks, D-01): whether TierPlan looks for newer images there and how often. Each field
// follows the instance default until it is changed here, and the schedule is a preset or a cron
// expression with its next checks, as the janitor's is. Edits are local until saved. Without
// updates.manage the fields are visible and read-only and say what they need (D-10); the server
// refuses a save either way.
export function UpdateChecksCard({ env }: UpdateChecksCardProps) {
  const t = useT();
  const id = useId();
  const status = useUpdateCheckStatus(env.id);
  const save = useSaveUpdateCheckPolicy(env.id);
  const needs = useNeeds("updates.manage", env.id);
  const canEdit = needs.allowed;
  const locked = canEdit
    ? undefined
    : t("updates.check.needs", {
        needs: needs.reason ?? t("needs.role.admin"),
        environment: env.name,
      });
  // What a reset field goes back to is the instance default. Reading it needs settings.read, which an
  // environment's Admin may not hold; then the field shows the value it had until the save answers.
  const canReadDefaults = usePermission("settings.read");
  const defaults = useUpdateCheckDefaults(canReadDefaults).data;

  // What the person has changed, and the status it was changed from. The form is drawn from this
  // snapshot while there are edits, so a read that lands in the middle of editing does not move what
  // a save would send or the version it carries (the server refuses a save made against another).
  const [draft, setDraft] = useState<Draft>({});
  const [base, setBase] = useState<UpdateCheckStatus | null>(null);
  const [failure, setFailure] = useState<Failure | null>(null);
  const [attempted, setAttempted] = useState(false);
  const [focusField, setFocusField] = useState<FieldName | null>(null);

  const data = base ?? status.data;
  const dirty = isDirty(draft);

  // The control a field's Reset returns focus to: the schedule's is its preset select.
  const controlId = (field: FieldName) =>
    field === "schedule" ? `${id}-schedule-field-preset` : `${id}-${field}`;
  useEffect(() => {
    if (!focusField) return;
    document.getElementById(controlId(focusField))?.focus();
    setFocusField(null);
  });

  function edit(field: FieldName, value: Draft[FieldName]) {
    if (!canEdit || !status.data) return;
    const snapshot = base ?? status.data;
    const next = withEdit(snapshot, defaults, draft, field, value);
    setBase(isDirty(next) ? snapshot : null);
    setDraft(next);
    setFailure(null);
  }

  function reset(field: FieldName) {
    edit(field, null);
    setFocusField(field);
  }

  function discard() {
    setDraft({});
    setBase(null);
    setFailure(null);
    setAttempted(false);
  }

  function reload() {
    discard();
    void status.refetch();
  }

  const view = data ? viewOf(data, defaults, draft) : undefined;
  const scheduleBlank = view !== undefined && cronOf(view.schedule.value) === "";

  const scheduleProblem: ScheduleProblem | null =
    failure?.kind === "schedule"
      ? failure.problem
      : attempted && scheduleBlank
        ? view?.schedule.value.preset === "custom"
          ? "invalid"
          : "time_missing"
        : null;

  async function saveNow() {
    if (!data || !view || save.isPending) return;
    try {
      await save.mutateAsync(fieldsOf(view, data.version));
      discard();
      toastSuccess(t("updates.checks.footer.saved"));
    } catch (e) {
      if (e instanceof UpdateSettingsError && e.type === "settings_changed") {
        setFailure({ kind: "conflict", by: conflictOf(e) });
      } else if (e instanceof UpdateSettingsError && e.type === "invalid_schedule") {
        setFailure({ kind: "schedule", problem: "invalid" });
      } else if (e instanceof UpdateSettingsError && e.type === "schedule_too_frequent") {
        setFailure({ kind: "schedule", problem: "too_frequent" });
      } else {
        setFailure({ kind: "failed" });
      }
    }
  }

  function press() {
    if (!canEdit || !dirty || save.isPending) return;
    if (scheduleBlank) {
      setAttempted(true);
      return;
    }
    void saveNow();
  }

  const enabledLabel = t("updates.checks.rows.enabled.label");
  const scheduleLabel = t("updates.checks.rows.schedule.label");

  const footerButton = (
    <Button
      type="button"
      variant="outline"
      className="max-md:h-11 max-md:w-full"
      aria-disabled={save.isPending || !dirty || undefined}
      onClick={press}
    >
      {save.isPending ? <Spinner aria-hidden="true" /> : null}
      {save.isPending ? t("updates.checks.footer.saving") : t("updates.checks.footer.save")}
    </Button>
  );

  return (
    <SettingsCard id="update-checks" title={t("updates.checks.title")}>
      <p className="text-sm text-muted-foreground">{t("updates.checks.caption")}</p>
      {status.data ? <StatusLine status={status.data} /> : null}
      {status.isPending ? (
        <div
          role="status"
          aria-label={t("updates.checks.loading")}
          aria-busy="true"
          className="flex flex-col gap-4"
        >
          {[0, 1].map((i) => (
            <Skeleton key={i} className="h-12 w-full" aria-hidden="true" />
          ))}
        </div>
      ) : !data || !view ? (
        <Alert variant="destructive">
          <AlertTitle>{t("updates.checks.loadFailed.title")}</AlertTitle>
          <AlertDescription className="flex flex-wrap items-center gap-3">
            <span>{t("updates.checks.loadFailed.body")}</span>
            <Button
              variant="outline"
              size="sm"
              disabled={status.isFetching}
              onClick={() => void status.refetch()}
            >
              {status.isFetching ? <Spinner aria-hidden="true" /> : null}
              {t("updates.checks.loadFailed.retry")}
            </Button>
          </AlertDescription>
        </Alert>
      ) : (
        <div className="flex flex-col">
          <PolicyRow
            id={`${id}-enabled-row`}
            label={enabledLabel}
            helper={t("updates.checks.rows.enabled.helper")}
            overridden={view.enabled.overridden}
            field={enabledLabel}
            onReset={() => reset("enabled")}
            locked={locked}
          >
            <div className="flex items-center gap-3">
              <NeedsTooltip reason={locked}>
                <Switch
                  id={controlId("enabled")}
                  checked={view.enabled.value}
                  aria-describedby={rowIds(`${id}-enabled-row`).helper}
                  onCheckedChange={(on) => edit("enabled", on)}
                />
              </NeedsTooltip>
              <label htmlFor={controlId("enabled")} className="text-sm">
                {t("updates.checks.rows.enabled.switch")}
              </label>
            </div>
          </PolicyRow>

          <PolicyRow
            id={`${id}-schedule-row`}
            label={scheduleLabel}
            overridden={view.schedule.overridden}
            field={scheduleLabel}
            onReset={() => reset("schedule")}
            locked={locked}
          >
            <ScheduleField
              scope="updates"
              envId={env.id}
              id={`${id}-schedule-field`}
              value={view.schedule.value}
              timezone={data.timezone}
              locked={locked}
              problem={scheduleProblem}
              onChange={(next) => edit("schedule", next)}
            />
          </PolicyRow>
        </div>
      )}

      {data && view ? (
        <div className="flex flex-col gap-3">
          {failure?.kind === "conflict" ? (
            <div role="alert" className="flex flex-wrap items-center gap-x-3 gap-y-2 text-sm">
              <p className="text-destructive">
                {failure.by
                  ? t("updates.checks.conflict.message", { user: failure.by })
                  : t("updates.checks.conflict.messageAnon")}
              </p>
              <Button
                type="button"
                variant="outline"
                className="max-md:h-11 max-md:w-full"
                onClick={reload}
              >
                {t("updates.checks.conflict.reload")}
              </Button>
            </div>
          ) : failure?.kind === "failed" ? (
            <p role="alert" className="text-sm text-destructive">
              {t("updates.checks.footer.saveFailed")} {t("updates.checks.footer.retry")}
            </p>
          ) : null}
          <div className="flex flex-wrap items-center gap-3">
            {canEdit ? footerButton : <NeedsTooltip reason={locked}>{footerButton}</NeedsTooltip>}
            <p role="status" className="text-xs text-muted-foreground">
              {dirty ? t("updates.checks.footer.unsaved") : ""}
            </p>
          </div>
        </div>
      ) : null}
    </SettingsCard>
  );
}
