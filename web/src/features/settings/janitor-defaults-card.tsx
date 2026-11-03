// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import { useId, useState } from "react";

import { environmentsQuery } from "@/api/environments";
import {
  conflictOf,
  type JanitorDefaults,
  JanitorRequestError,
  useJanitorDefaults,
  useRetentionPreview,
  useSaveJanitorDefaults,
} from "@/api/janitor";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
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
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { Switch } from "@/components/ui/switch";
import {
  couldRemoveMore,
  cronOf,
  GRACE_MAX,
  GRACE_MIN,
  KEEP_MAX,
  KEEP_MIN,
  type PolicyValues,
  parseWhole,
  type ScheduleDraft,
  scheduleDraftOf,
} from "@/features/janitor/policy-diff";
import { PolicyRow, rowIds } from "@/features/janitor/policy-row";
import { ScheduleField, type ScheduleProblem } from "@/features/janitor/schedule-field";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { toastSuccess } from "@/lib/toast";

// The run-history ranges the server enforces again (D-16).
const DAYS_MIN = 7;
const DAYS_MAX = 3650;
const RUNS_MIN = 10;
const RUNS_MAX = 1000;

// What a new instance starts with (D-03), named in the helpers of the two rows that say "Default {n}".
const FACTORY_KEEP = 2;
const FACTORY_GRACE_HOURS = 24;

// The defaults as the form holds them: the numbers are the text of their fields, because a
// half-typed number is still being edited.
interface Form {
  enabled: boolean;
  schedule: ScheduleDraft;
  keepN: string;
  graceHours: string;
  removeUnusedNetworks: boolean;
  removeStoppedContainers: boolean;
  retentionDays: string;
  retentionRuns: string;
}

function formOf(d: JanitorDefaults): Form {
  return {
    enabled: d.enabled,
    schedule: scheduleDraftOf(d.schedule, d.schedulePreset),
    keepN: String(d.keepN),
    graceHours: String(d.graceHours),
    removeUnusedNetworks: d.removeUnusedNetworks,
    removeStoppedContainers: d.removeStoppedContainers,
    retentionDays: String(d.retentionDays),
    retentionRuns: String(d.retentionRuns),
  };
}

// policyValuesOf is the six policy fields as numbers, for asking whether a change could make the
// janitor remove more. A field that is not a whole number in range is NaN, which is never lower than
// anything: the card does not compare or save such a form.
function policyValuesOf(f: Form): PolicyValues {
  return {
    enabled: f.enabled,
    schedule: cronOf(f.schedule),
    keepN: parseWhole(f.keepN, KEEP_MIN, KEEP_MAX) ?? Number.NaN,
    graceHours: parseWhole(f.graceHours, GRACE_MIN, GRACE_MAX) ?? Number.NaN,
    removeUnusedNetworks: f.removeUnusedNetworks,
    removeStoppedContainers: f.removeStoppedContainers,
  };
}

function sameForm(a: Form, b: Form): boolean {
  return (
    a.enabled === b.enabled &&
    a.schedule.preset === b.schedule.preset &&
    cronOf(a.schedule) === cronOf(b.schedule) &&
    a.keepN.trim() === b.keepN.trim() &&
    a.graceHours.trim() === b.graceHours.trim() &&
    a.removeUnusedNetworks === b.removeUnusedNetworks &&
    a.removeStoppedContainers === b.removeStoppedContainers &&
    a.retentionDays.trim() === b.retentionDays.trim() &&
    a.retentionRuns.trim() === b.retentionRuns.trim()
  );
}

type Failure =
  | { kind: "conflict"; by: string }
  | { kind: "schedule"; problem: ScheduleProblem }
  | { kind: "preview" }
  | { kind: "failed" };

// What a lowered retention would delete now, as its confirm names it.
interface Removal {
  runs: number;
  environments: number;
}

interface FormProps {
  // The defaults as read when editing began. A save sends this snapshot's version, so a background
  // read cannot hide a conflict from the server's 409.
  base: JanitorDefaults;
  // The environment whose schedule-preview route answers for a custom expression.
  previewEnvId?: string;
  locked?: string;
  onSaved: () => void;
  onReload: () => void;
}

function JanitorDefaultsForm({ base, previewEnvId, locked, onSaved, onReload }: FormProps) {
  const t = useT();
  const id = useId();
  const save = useSaveJanitorDefaults();
  const countRetention = useRetentionPreview();
  const canEdit = locked === undefined;

  const saved = formOf(base);
  const [form, setForm] = useState<Form>(saved);
  const [failure, setFailure] = useState<Failure | null>(null);
  const [attempted, setAttempted] = useState(false);
  // checking: the count a lowered retention would delete is being asked for.
  const [checking, setChecking] = useState(false);
  const [removal, setRemoval] = useState<Removal | null>(null);
  const [removalOpen, setRemovalOpen] = useState(false);
  const [defaultsOpen, setDefaultsOpen] = useState(false);

  const dirty = !sameForm(form, saved);
  const busy = save.isPending || checking;

  function edit(patch: Partial<Form>) {
    if (!canEdit) return;
    setForm((f) => ({ ...f, ...patch }));
    setFailure(null);
  }

  const days = parseWhole(form.retentionDays, DAYS_MIN, DAYS_MAX);
  const runs = parseWhole(form.retentionRuns, RUNS_MIN, RUNS_MAX);
  const keepValid = parseWhole(form.keepN, KEEP_MIN, KEEP_MAX) !== null;
  const graceValid = parseWhole(form.graceHours, GRACE_MIN, GRACE_MAX) !== null;
  const scheduleBlank = cronOf(form.schedule) === "";
  const invalid = !keepValid || !graceValid || days === null || runs === null || scheduleBlank;

  const scheduleProblem: ScheduleProblem | null =
    failure?.kind === "schedule"
      ? failure.problem
      : attempted && scheduleBlank
        ? form.schedule.preset === "custom"
          ? "invalid"
          : "time_missing"
        : null;

  async function commit() {
    if (invalid || days === null || runs === null || save.isPending) return;
    try {
      await save.mutateAsync({
        version: base.version,
        enabled: form.enabled,
        schedule: cronOf(form.schedule),
        schedulePreset: form.schedule.preset,
        keepN: Number(form.keepN.trim()),
        graceHours: Number(form.graceHours.trim()),
        removeUnusedNetworks: form.removeUnusedNetworks,
        removeStoppedContainers: form.removeStoppedContainers,
        retentionDays: days,
        retentionRuns: runs,
      });
      toastSuccess(t("janitorDefaults.saved"));
      onSaved();
    } catch (e) {
      if (e instanceof JanitorRequestError && e.type === "settings_changed") {
        setFailure({ kind: "conflict", by: conflictOf(e) });
      } else if (e instanceof JanitorRequestError && e.type === "invalid_schedule") {
        setFailure({ kind: "schedule", problem: "invalid" });
      } else if (e instanceof JanitorRequestError && e.type === "schedule_too_frequent") {
        setFailure({ kind: "schedule", problem: "too_frequent" });
      } else {
        setFailure({ kind: "failed" });
      }
    }
  }

  // Widening the defaults changes what every environment that follows them may remove, so it is
  // confirmed with how many follow. With none following, nothing is widened for anyone.
  function afterRetention() {
    const widens = couldRemoveMore(policyValuesOf(saved), policyValuesOf(form));
    if (widens && base.followingEnvironments > 0) setDefaultsOpen(true);
    else void commit();
  }

  async function press() {
    if (!canEdit || !dirty || busy) return;
    if (invalid || days === null || runs === null) {
      setAttempted(true);
      return;
    }
    setFailure(null);
    // Raising a limit deletes nothing. Lowering one asks the server how many runs would go, so the
    // confirm can say so; when none would, there is nothing to confirm.
    if (days < base.retentionDays || runs < base.retentionRuns) {
      setChecking(true);
      try {
        const count = await countRetention(days, runs);
        if (count.runs > 0) {
          setRemoval({ runs: count.runs, environments: count.environments });
          setRemovalOpen(true);
          return;
        }
      } catch {
        // Saving blind could delete history nobody was told about, so nothing is saved.
        setFailure({ kind: "preview" });
        return;
      } finally {
        setChecking(false);
      }
    }
    afterRetention();
  }

  const ids = (row: string) => rowIds(`${id}-${row}`);
  const readOnlyProps = { readOnly: !canEdit } as const;
  const daysError = days === null ? t("janitorDefaults.retention.days.error") : undefined;
  const runsError = runs === null ? t("janitorDefaults.retention.runs.error") : undefined;

  const footerButton = (
    <Button
      type="button"
      variant="outline"
      className="max-md:h-11 max-md:w-full"
      aria-disabled={busy || !dirty || undefined}
      onClick={() => void press()}
    >
      {busy ? <Spinner aria-hidden="true" /> : null}
      {save.isPending
        ? t("janitorDefaults.footer.saving")
        : checking
          ? t("janitorDefaults.footer.checking")
          : t("janitorDefaults.footer.save")}
    </Button>
  );

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-col">
        <PolicyRow
          id={`${id}-enabled`}
          label={t("janitorPolicy.rows.enabled.label")}
          helper={t("janitorPolicy.rows.enabled.helper")}
        >
          <div className="flex items-center gap-3">
            <NeedsTooltip reason={locked}>
              <Switch
                id={`${id}-enabled-switch`}
                checked={form.enabled}
                aria-describedby={ids("enabled").helper}
                onCheckedChange={(on) => edit({ enabled: on })}
              />
            </NeedsTooltip>
            <label htmlFor={`${id}-enabled-switch`} className="text-sm">
              {t("janitorDefaults.enabledSwitch")}
            </label>
          </div>
        </PolicyRow>

        <PolicyRow id={`${id}-schedule`} label={t("janitorPolicy.schedule.label")}>
          <ScheduleField
            envId={previewEnvId}
            id={`${id}-schedule-field`}
            value={form.schedule}
            timezone={base.timezone}
            locked={locked}
            problem={scheduleProblem}
            onChange={(schedule) => edit({ schedule })}
          />
        </PolicyRow>

        <PolicyRow
          id={`${id}-keep`}
          label={t("janitorPolicy.rows.keep.label")}
          labelFor={`${id}-keepN`}
          helper={t("janitorPolicy.rows.keep.helper", { default: FACTORY_KEEP })}
          error={keepValid ? undefined : t("janitorPolicy.rows.keep.error")}
        >
          <NeedsTooltip reason={locked}>
            <Input
              id={`${id}-keepN`}
              type="number"
              inputMode="numeric"
              min={KEEP_MIN}
              max={KEEP_MAX}
              step={1}
              value={form.keepN}
              {...readOnlyProps}
              aria-invalid={!keepValid || undefined}
              aria-describedby={`${ids("keep").helper} ${ids("keep").error}`}
              className="w-28 max-md:h-11"
              onChange={(e) => edit({ keepN: e.target.value })}
            />
          </NeedsTooltip>
        </PolicyRow>

        <PolicyRow
          id={`${id}-grace`}
          label={t("janitorPolicy.rows.grace.label")}
          labelFor={`${id}-graceHours`}
          helper={t("janitorPolicy.rows.grace.helper", {
            default: t("janitor.age.hours", { count: FACTORY_GRACE_HOURS }),
          })}
          error={graceValid ? undefined : t("janitorPolicy.rows.grace.error")}
        >
          <div className="flex items-center gap-2">
            <NeedsTooltip reason={locked}>
              <Input
                id={`${id}-graceHours`}
                type="number"
                inputMode="numeric"
                min={GRACE_MIN}
                max={GRACE_MAX}
                step={1}
                value={form.graceHours}
                {...readOnlyProps}
                aria-invalid={!graceValid || undefined}
                aria-describedby={`${ids("grace").helper} ${ids("grace").error}`}
                className="w-28 max-md:h-11"
                onChange={(e) => edit({ graceHours: e.target.value })}
              />
            </NeedsTooltip>
            <span className="text-sm text-muted-foreground">
              {t("janitorPolicy.rows.grace.suffix")}
            </span>
          </div>
        </PolicyRow>

        <PolicyRow
          id={`${id}-networks`}
          label={t("janitorPolicy.rows.networks.label")}
          helper={t("janitorPolicy.rows.networks.helper")}
        >
          <NeedsTooltip reason={locked}>
            <Switch
              checked={form.removeUnusedNetworks}
              aria-labelledby={ids("networks").title}
              aria-describedby={ids("networks").helper}
              onCheckedChange={(on) => edit({ removeUnusedNetworks: on })}
            />
          </NeedsTooltip>
        </PolicyRow>

        <PolicyRow
          id={`${id}-containers`}
          label={t("janitorPolicy.rows.containers.label")}
          helper={t("janitorPolicy.rows.containers.helper")}
        >
          <NeedsTooltip reason={locked}>
            <Switch
              checked={form.removeStoppedContainers}
              aria-labelledby={ids("containers").title}
              aria-describedby={ids("containers").helper}
              onCheckedChange={(on) => edit({ removeStoppedContainers: on })}
            />
          </NeedsTooltip>
        </PolicyRow>

        <PolicyRow
          id={`${id}-days`}
          label={t("janitorDefaults.retention.days.label")}
          labelFor={`${id}-retentionDays`}
          helper={t("janitorDefaults.retention.helper")}
          error={daysError}
        >
          <div className="flex items-center gap-2">
            <NeedsTooltip reason={locked}>
              <Input
                id={`${id}-retentionDays`}
                type="number"
                inputMode="numeric"
                min={DAYS_MIN}
                max={DAYS_MAX}
                step={1}
                value={form.retentionDays}
                {...readOnlyProps}
                aria-invalid={days === null || undefined}
                aria-describedby={`${ids("days").helper} ${ids("days").error}`}
                className="w-24 max-md:h-11"
                onChange={(e) => edit({ retentionDays: e.target.value })}
              />
            </NeedsTooltip>
            <span className="text-sm text-muted-foreground">
              {t("janitorDefaults.retention.days.suffix")}
            </span>
          </div>
        </PolicyRow>

        <PolicyRow
          id={`${id}-runs`}
          label={t("janitorDefaults.retention.runs.label")}
          labelFor={`${id}-retentionRuns`}
          error={runsError}
        >
          <div className="flex items-center gap-2">
            <NeedsTooltip reason={locked}>
              <Input
                id={`${id}-retentionRuns`}
                type="number"
                inputMode="numeric"
                min={RUNS_MIN}
                max={RUNS_MAX}
                step={1}
                value={form.retentionRuns}
                {...readOnlyProps}
                aria-invalid={runs === null || undefined}
                aria-describedby={`${ids("days").helper} ${ids("runs").error}`}
                className="w-24 max-md:h-11"
                onChange={(e) => edit({ retentionRuns: e.target.value })}
              />
            </NeedsTooltip>
            <span className="text-sm text-muted-foreground">
              {t("janitorDefaults.retention.runs.suffix")}
            </span>
          </div>
        </PolicyRow>
      </div>

      <div className="flex flex-col gap-3">
        {failure?.kind === "conflict" ? (
          <div role="alert" className="flex flex-wrap items-center gap-x-3 gap-y-2 text-sm">
            <p className="text-destructive">
              {failure.by
                ? t("janitorDefaults.conflict.message", { user: failure.by })
                : t("janitorDefaults.conflict.messageAnon")}
            </p>
            <Button
              type="button"
              variant="outline"
              className="max-md:h-11 max-md:w-full"
              onClick={onReload}
            >
              {t("janitorDefaults.conflict.reload")}
            </Button>
          </div>
        ) : failure?.kind === "failed" ? (
          <p role="alert" className="text-sm text-destructive">
            {t("janitorDefaults.saveFailed")} {t("janitorDefaults.retry")}
          </p>
        ) : failure?.kind === "preview" ? (
          <p role="alert" className="text-sm text-destructive">
            {t("janitorDefaults.previewFailed")} {t("janitorDefaults.retry")}
          </p>
        ) : null}
        <div className="flex flex-wrap items-center gap-3">
          {canEdit ? footerButton : <NeedsTooltip reason={locked}>{footerButton}</NeedsTooltip>}
          <p role="status" className="text-xs text-muted-foreground">
            {dirty ? t("janitorDefaults.footer.unsaved") : ""}
          </p>
        </div>
      </div>

      <AlertDialog open={removalOpen} onOpenChange={setRemovalOpen}>
        <AlertDialogContent className="sm:max-w-120">
          <AlertDialogHeader>
            <AlertDialogTitle>{t("janitorDefaults.retentionConfirm.title")}</AlertDialogTitle>
            <AlertDialogDescription>
              {removal
                ? t("janitorDefaults.retentionConfirm.body", {
                    runs: t("janitorDefaults.retentionConfirm.runs", { count: removal.runs }),
                    environments: t("janitorDefaults.retentionConfirm.environments", {
                      count: removal.environments,
                    }),
                  })
                : null}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel autoFocus className="max-md:h-11">
              {t("janitorDefaults.retentionConfirm.cancel")}
            </AlertDialogCancel>
            <AlertDialogAction
              variant="destructive"
              className="max-md:h-11"
              onClick={() => {
                setRemovalOpen(false);
                afterRetention();
              }}
            >
              {t("janitorDefaults.retentionConfirm.remove", { count: removal?.runs ?? 0 })}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <AlertDialog open={defaultsOpen} onOpenChange={setDefaultsOpen}>
        <AlertDialogContent className="sm:max-w-120">
          <AlertDialogHeader>
            <AlertDialogTitle>{t("janitorDefaults.defaultsConfirm.title")}</AlertDialogTitle>
            <AlertDialogDescription>
              {t("janitorDefaults.defaultsConfirm.body", { count: base.followingEnvironments })}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel autoFocus className="max-md:h-11">
              {t("janitorDefaults.defaultsConfirm.cancel")}
            </AlertDialogCancel>
            <AlertDialogAction
              className="max-md:h-11"
              onClick={() => {
                setDefaultsOpen(false);
                void commit();
              }}
            >
              {t("janitorDefaults.defaultsConfirm.save")}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

// JanitorDefaultsCard is the "Janitor defaults" card of Settings > General (anchor #janitor-defaults,
// JAN-01, JAN-10, D-04, D-16): what every environment's janitor does until the environment sets the
// field itself, and how long the run history is kept. Reading needs settings.read and changing
// settings.write, which only an instance admin holds (D-21); the server checks both again. A change
// that could make the janitor remove more is confirmed with how many environments follow the
// defaults, and a lowered retention that deletes runs is confirmed with the count the server gives.
export function JanitorDefaultsCard() {
  const t = useT();
  const defaults = useJanitorDefaults();
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
    <section id="janitor-defaults" aria-label={t("janitorDefaults.title")} className="scroll-mt-4">
      <Card className="max-w-4xl">
        <CardHeader>
          <CardTitle>
            <h2>{t("janitorDefaults.title")}</h2>
          </CardTitle>
          <CardDescription>{t("janitorDefaults.caption")}</CardDescription>
        </CardHeader>
        <CardContent>
          {defaults.data ? (
            <JanitorDefaultsForm
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
              aria-label={t("janitorDefaults.loading")}
              aria-busy="true"
              className="flex flex-col gap-4"
            >
              {[0, 1, 2, 3].map((i) => (
                <Skeleton key={i} className="h-12 w-full" aria-hidden="true" />
              ))}
            </div>
          ) : (
            <Alert variant="destructive">
              <AlertTitle>{t("janitorDefaults.loadFailed.title")}</AlertTitle>
              <AlertDescription className="flex flex-wrap items-center gap-3">
                <span>{t("janitorDefaults.loadFailed.body")}</span>
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  disabled={defaults.isFetching}
                  onClick={() => void defaults.refetch()}
                >
                  {defaults.isFetching ? <Spinner aria-hidden="true" /> : null}
                  {t("janitorDefaults.loadFailed.retry")}
                </Button>
              </AlertDescription>
            </Alert>
          )}
        </CardContent>
      </Card>
    </section>
  );
}
