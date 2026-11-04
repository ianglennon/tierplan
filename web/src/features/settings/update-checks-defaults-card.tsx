// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import { useId, useState } from "react";

import { environmentsQuery } from "@/api/environments";
import {
  conflictOf,
  type UpdateCheckDefaults,
  UpdateSettingsError,
  useSaveUpdateCheckDefaults,
  useUpdateCheckDefaults,
} from "@/api/updates";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { Switch } from "@/components/ui/switch";
import { cronOf, type ScheduleDraft, scheduleDraftOf } from "@/features/janitor/policy-diff";
import { PolicyRow, rowIds } from "@/features/janitor/policy-row";
import { ScheduleField, type ScheduleProblem } from "@/features/janitor/schedule-field";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { toastSuccess } from "@/lib/toast";

// The defaults as the form holds them.
interface Form {
  enabled: boolean;
  schedule: ScheduleDraft;
}

function formOf(d: UpdateCheckDefaults): Form {
  return { enabled: d.enabled, schedule: scheduleDraftOf(d.schedule, d.preset) };
}

const sameForm = (a: Form, b: Form) =>
  a.enabled === b.enabled &&
  a.schedule.preset === b.schedule.preset &&
  cronOf(a.schedule) === cronOf(b.schedule);

type Failure =
  | { kind: "conflict"; by: string }
  | { kind: "schedule"; problem: ScheduleProblem }
  | { kind: "failed" };

interface FormProps {
  // The defaults as read when editing began. A save sends this snapshot's version, so a background
  // read cannot hide a conflict from the server's 409.
  base: UpdateCheckDefaults;
  // The environment whose schedule-preview route answers for a custom expression.
  previewEnvId?: string;
  locked?: string;
  onSaved: () => void;
  onReload: () => void;
}

function UpdateChecksDefaultsForm({ base, previewEnvId, locked, onSaved, onReload }: FormProps) {
  const t = useT();
  const id = useId();
  const save = useSaveUpdateCheckDefaults();
  const canEdit = locked === undefined;

  const saved = formOf(base);
  const [form, setForm] = useState<Form>(saved);
  const [failure, setFailure] = useState<Failure | null>(null);
  const [attempted, setAttempted] = useState(false);

  const dirty = !sameForm(form, saved);

  function edit(patch: Partial<Form>) {
    if (!canEdit) return;
    setForm((f) => ({ ...f, ...patch }));
    setFailure(null);
  }

  const scheduleBlank = cronOf(form.schedule) === "";
  const scheduleProblem: ScheduleProblem | null =
    failure?.kind === "schedule"
      ? failure.problem
      : attempted && scheduleBlank
        ? form.schedule.preset === "custom"
          ? "invalid"
          : "time_missing"
        : null;

  async function press() {
    if (!canEdit || !dirty || save.isPending) return;
    if (scheduleBlank) {
      setAttempted(true);
      return;
    }
    setFailure(null);
    try {
      await save.mutateAsync({
        version: base.version,
        enabled: form.enabled,
        schedule: cronOf(form.schedule),
        preset: form.schedule.preset,
      });
      toastSuccess(t("updates.defaults.saved"));
      onSaved();
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

  const footerButton = (
    <Button
      type="button"
      variant="outline"
      className="max-md:h-11 max-md:w-full"
      aria-disabled={save.isPending || !dirty || undefined}
      onClick={() => void press()}
    >
      {save.isPending ? <Spinner aria-hidden="true" /> : null}
      {save.isPending ? t("updates.defaults.footer.saving") : t("updates.defaults.footer.save")}
    </Button>
  );

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-col">
        <PolicyRow
          id={`${id}-enabled`}
          label={t("updates.checks.rows.enabled.label")}
          helper={t("updates.checks.rows.enabled.helper")}
        >
          <div className="flex items-center gap-3">
            <NeedsTooltip reason={locked}>
              <Switch
                id={`${id}-enabled-switch`}
                checked={form.enabled}
                aria-describedby={rowIds(`${id}-enabled`).helper}
                onCheckedChange={(on) => edit({ enabled: on })}
              />
            </NeedsTooltip>
            <label htmlFor={`${id}-enabled-switch`} className="text-sm">
              {t("updates.defaults.enabledSwitch")}
            </label>
          </div>
        </PolicyRow>

        <PolicyRow id={`${id}-schedule`} label={t("updates.checks.rows.schedule.label")}>
          <ScheduleField
            scope="updates"
            envId={previewEnvId}
            id={`${id}-schedule-field`}
            value={form.schedule}
            timezone={base.timezone}
            locked={locked}
            problem={scheduleProblem}
            onChange={(schedule) => edit({ schedule })}
          />
        </PolicyRow>
      </div>

      <div className="flex flex-col gap-3">
        {failure?.kind === "conflict" ? (
          <div role="alert" className="flex flex-wrap items-center gap-x-3 gap-y-2 text-sm">
            <p className="text-destructive">
              {failure.by
                ? t("updates.defaults.conflict.message", { user: failure.by })
                : t("updates.defaults.conflict.messageAnon")}
            </p>
            <Button
              type="button"
              variant="outline"
              className="max-md:h-11 max-md:w-full"
              onClick={onReload}
            >
              {t("updates.defaults.conflict.reload")}
            </Button>
          </div>
        ) : failure?.kind === "failed" ? (
          <p role="alert" className="text-sm text-destructive">
            {t("updates.defaults.saveFailed")} {t("updates.defaults.retry")}
          </p>
        ) : null}
        <div className="flex flex-wrap items-center gap-3">
          {canEdit ? footerButton : <NeedsTooltip reason={locked}>{footerButton}</NeedsTooltip>}
          <p role="status" className="text-xs text-muted-foreground">
            {dirty ? t("updates.defaults.footer.unsaved") : ""}
          </p>
        </div>
      </div>
    </div>
  );
}

// UpdateChecksDefaultsCard is the "Image update check defaults" card of Settings > General (anchor
// #update-checks, D-01, D-22): whether environments are checked for newer images and how often, until
// an environment sets the field itself. Reading needs settings.read and changing settings.write; the
// server checks both again. There is no confirm: a change applies from the next check and nothing is
// removed.
export function UpdateChecksDefaultsCard() {
  const t = useT();
  const defaults = useUpdateCheckDefaults();
  const needs = useNeeds("settings.write");
  const locked = needs.allowed ? undefined : (needs.reason ?? t("needs.instance"));
  // The schedule-preview route belongs to an environment, so a custom expression is checked against
  // the first environment the person may read.
  const environments = useQuery(environmentsQuery);
  const previewEnvId = environments.data?.items?.[0]?.id;
  // A new key starts the form again from the defaults as they now are: after a save, and on Reload.
  const [generation, setGeneration] = useState(0);

  async function reload() {
    await defaults.refetch();
    setGeneration((g) => g + 1);
  }

  return (
    <section id="update-checks" aria-label={t("updates.defaults.title")} className="scroll-mt-4">
      <Card className="max-w-4xl">
        <CardHeader>
          <CardTitle>
            <h2>{t("updates.defaults.title")}</h2>
          </CardTitle>
          <CardDescription>{t("updates.defaults.caption")}</CardDescription>
        </CardHeader>
        <CardContent>
          {defaults.data ? (
            <UpdateChecksDefaultsForm
              key={generation}
              base={defaults.data}
              previewEnvId={previewEnvId}
              locked={locked}
              onSaved={() => setGeneration((g) => g + 1)}
              onReload={() => void reload()}
            />
          ) : defaults.isPending ? (
            <div
              role="status"
              aria-label={t("updates.defaults.loading")}
              aria-busy="true"
              className="flex flex-col gap-4"
            >
              {[0, 1].map((i) => (
                <Skeleton key={i} className="h-12 w-full" aria-hidden="true" />
              ))}
            </div>
          ) : (
            <Alert variant="destructive">
              <AlertTitle>{t("updates.defaults.loadFailed.title")}</AlertTitle>
              <AlertDescription className="flex flex-wrap items-center gap-3">
                <span>{t("updates.defaults.loadFailed.body")}</span>
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  disabled={defaults.isFetching}
                  onClick={() => void defaults.refetch()}
                >
                  {defaults.isFetching ? <Spinner aria-hidden="true" /> : null}
                  {t("updates.defaults.loadFailed.retry")}
                </Button>
              </AlertDescription>
            </Alert>
          )}
        </CardContent>
      </Card>
    </section>
  );
}
