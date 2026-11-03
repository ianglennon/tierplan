// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useBlocker } from "@tanstack/react-router";
import { useEffect, useId, useRef, useState } from "react";

import {
  conflictOf,
  type JanitorPolicy,
  JanitorRequestError,
  useJanitorPolicy,
  useSaveJanitorPolicy,
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
import { CleanupPreviewSheet } from "@/features/janitor/cleanup-preview-sheet";
import { NodeOverridesTable } from "@/features/janitor/node-overrides-table";
import {
  couldRemoveMore,
  cronOf,
  type Draft,
  type FieldName,
  fieldsOf,
  GRACE_MAX,
  GRACE_MIN,
  isDirty,
  KEEP_MAX,
  KEEP_MIN,
  parseWhole,
  valuesOf,
  viewOf,
  withEdit,
} from "@/features/janitor/policy-diff";
import { PolicyRow, rowIds } from "@/features/janitor/policy-row";
import { RulesNote } from "@/features/janitor/rules-note";
import { ScheduleField, type ScheduleProblem } from "@/features/janitor/schedule-field";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { toastSuccess } from "@/lib/toast";

type Failure =
  | { kind: "conflict"; by: string }
  | { kind: "schedule"; problem: ScheduleProblem }
  | { kind: "failed" };

interface PolicyCardProps {
  envId: string;
  envName: string;
  // True on a swarm: the keep-newest row speaks of task containers, and the preview groups by node.
  swarm: boolean;
  // The nodes the environment should have, for the preview's "Checking {n} nodes…" line.
  nodeCount: number;
  // Whether the janitor has run once, for the Rules note, which is open until it has. Undefined
  // while the status is not read, and the note waits for it.
  firstRunDone?: boolean;
}

// PolicyCard is the environment's janitor policy (anchor #policy, D-04, D-11): each field follows the
// instance default until it is changed here, the schedule is a preset or a cron expression with its
// next runs, and a change that could make the janitor remove more is shown in the preview before it is
// saved (JAN-08). Edits are local until saved; leaving the page with unsaved edits asks first.
// Without janitor.policy every field is visible and read-only and says what it needs (D-15, D-21).
export function PolicyCard({ envId, envName, swarm, nodeCount, firstRunDone }: PolicyCardProps) {
  const t = useT();
  const id = useId();
  const policy = useJanitorPolicy(envId);
  const save = useSaveJanitorPolicy(envId);
  const needs = useNeeds("janitor.policy", envId);
  const canEdit = needs.allowed;
  const locked = canEdit
    ? undefined
    : t("janitor.needsOn", { need: needs.reason ?? t("needs.role.admin"), name: envName });

  // What the person has changed, and the policy it was changed from. The form is drawn from this
  // snapshot while there are edits, so a read that lands in the middle of editing does not move what
  // a save would send or the version it carries (the server refuses a save made against another).
  const [draft, setDraft] = useState<Draft>({});
  const [base, setBase] = useState<JanitorPolicy | null>(null);
  const [failure, setFailure] = useState<Failure | null>(null);
  const [attempted, setAttempted] = useState(false);
  const [reviewing, setReviewing] = useState(false);
  const [focusField, setFocusField] = useState<FieldName | null>(null);

  const data = base ?? policy.data;
  const dirty = isDirty(draft);

  // The control a field's Reset returns focus to: the schedule's is its preset select.
  const controlId = (field: FieldName) =>
    field === "schedule" ? `${id}-schedule-preset` : `${id}-${field}`;
  useEffect(() => {
    if (!focusField) return;
    document.getElementById(controlId(focusField))?.focus();
    setFocusField(null);
  });

  const dirtyRef = useRef(false);
  dirtyRef.current = dirty;
  const blocker = useBlocker({
    shouldBlockFn: ({ current, next }) => dirtyRef.current && current.pathname !== next.pathname,
    enableBeforeUnload: () => dirtyRef.current,
    withResolver: true,
  });

  function edit(field: FieldName, value: Draft[FieldName]) {
    if (!canEdit || !policy.data) return;
    const snapshot = base ?? policy.data;
    const next = withEdit(snapshot, draft, field, value);
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
    setReviewing(false);
  }

  function reload() {
    discard();
    void policy.refetch();
  }

  const view = data ? viewOf(data, draft) : undefined;
  const keepValid = view ? parseWhole(view.keepN.value, KEEP_MIN, KEEP_MAX) !== null : true;
  const graceValid = view ? parseWhole(view.graceHours.value, GRACE_MIN, GRACE_MAX) !== null : true;
  const cron = view ? cronOf(view.schedule.value) : "";
  const scheduleBlank = view !== undefined && cron === "";
  const invalid = !keepValid || !graceValid || scheduleBlank;
  const review =
    dirty && !invalid && data !== undefined && view !== undefined
      ? couldRemoveMore(valuesOf(viewOf(data, {})), valuesOf(view))
      : false;

  const scheduleProblem: ScheduleProblem | null =
    failure?.kind === "schedule"
      ? failure.problem
      : attempted && scheduleBlank
        ? view?.schedule.value.preset === "custom"
          ? "invalid"
          : "time_missing"
        : null;

  // save stores the form. It answers whether the policy was saved, for the preview sheet to close
  // on; a refusal is worded on the card, which is what the person sees once the sheet is gone.
  async function saveNow(): Promise<boolean> {
    if (!data || !view || save.isPending) return false;
    try {
      await save.mutateAsync({ ...fieldsOf(view), version: data.version });
      discard();
      toastSuccess(t("janitorPolicy.footer.saved"));
      return true;
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
      return false;
    }
  }

  function press() {
    if (!canEdit || !dirty || save.isPending) return;
    if (invalid) {
      setAttempted(true);
      return;
    }
    if (review) setReviewing(true);
    else void saveNow();
  }

  const keep = data?.keepN.value;
  const grace = data?.graceHours.value;

  const keepLabel = swarm
    ? t("janitorPolicy.rows.keep.label")
    : t("janitorPolicy.rows.keep.labelStandalone");
  const graceLabel = t("janitorPolicy.rows.grace.label");
  const enabledLabel = t("janitorPolicy.rows.enabled.label");
  const scheduleLabel = t("janitorPolicy.schedule.label");
  const networksLabel = t("janitorPolicy.rows.networks.label");
  const containersLabel = t("janitorPolicy.rows.containers.label");

  const footerLabel = save.isPending
    ? t("janitorPolicy.footer.saving")
    : review
      ? t("janitorPolicy.footer.review")
      : t("janitorPolicy.footer.save");
  const footerButton = (
    <Button
      type="button"
      variant="outline"
      className="max-md:h-11 max-md:w-full"
      aria-disabled={save.isPending || !dirty || undefined}
      onClick={press}
    >
      {save.isPending ? <Spinner aria-hidden="true" /> : null}
      {footerLabel}
    </Button>
  );

  return (
    <section id="policy" aria-label={t("janitorPolicy.title")} className="scroll-mt-4">
      <Card>
        <CardHeader>
          <CardTitle>{t("janitorPolicy.title")}</CardTitle>
          <CardDescription>{t("janitorPolicy.caption")}</CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-4">
          {policy.isPending ? (
            <div
              role="status"
              aria-label={t("janitorPolicy.loading")}
              aria-busy="true"
              className="flex flex-col gap-4"
            >
              {[0, 1, 2, 3].map((i) => (
                <Skeleton key={i} className="h-12 w-full" aria-hidden="true" />
              ))}
            </div>
          ) : !data || !view ? (
            <Alert variant="destructive">
              <AlertTitle>{t("janitorPolicy.loadFailed.title")}</AlertTitle>
              <AlertDescription className="flex flex-wrap items-center gap-3">
                <span>{t("janitorPolicy.loadFailed.body")}</span>
                <Button
                  variant="outline"
                  size="sm"
                  disabled={policy.isFetching}
                  onClick={() => void policy.refetch()}
                >
                  {policy.isFetching ? <Spinner aria-hidden="true" /> : null}
                  {t("janitorPolicy.loadFailed.retry")}
                </Button>
              </AlertDescription>
            </Alert>
          ) : (
            <div className="flex flex-col">
              <PolicyRow
                id={`${id}-enabled-row`}
                label={enabledLabel}
                helper={t("janitorPolicy.rows.enabled.helper")}
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
                    {t("janitorPolicy.rows.enabled.switch")}
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
                  envId={envId}
                  id={`${id}-schedule`}
                  value={view.schedule.value}
                  timezone={data.timezone}
                  locked={locked}
                  problem={scheduleProblem}
                  onChange={(next) => edit("schedule", next)}
                />
              </PolicyRow>

              <PolicyRow
                id={`${id}-keep-row`}
                label={keepLabel}
                labelFor={controlId("keepN")}
                helper={t("janitorPolicy.rows.keep.helper", { default: data.defaults.keepN })}
                overridden={view.keepN.overridden}
                field={keepLabel}
                onReset={() => reset("keepN")}
                locked={locked}
                error={keepValid ? undefined : t("janitorPolicy.rows.keep.error")}
              >
                <NeedsTooltip reason={locked}>
                  <Input
                    id={controlId("keepN")}
                    type="number"
                    inputMode="numeric"
                    min={KEEP_MIN}
                    max={KEEP_MAX}
                    step={1}
                    value={view.keepN.value}
                    readOnly={!canEdit}
                    aria-invalid={!keepValid || undefined}
                    aria-describedby={`${rowIds(`${id}-keep-row`).helper} ${rowIds(`${id}-keep-row`).error}`}
                    className="w-28 max-md:h-11"
                    onChange={(e) => edit("keepN", e.target.value)}
                  />
                </NeedsTooltip>
              </PolicyRow>

              <PolicyRow
                id={`${id}-grace-row`}
                label={graceLabel}
                labelFor={controlId("graceHours")}
                helper={t("janitorPolicy.rows.grace.helper", {
                  default: t("janitor.age.hours", { count: data.defaults.graceHours }),
                })}
                overridden={view.graceHours.overridden}
                field={graceLabel}
                onReset={() => reset("graceHours")}
                locked={locked}
                error={graceValid ? undefined : t("janitorPolicy.rows.grace.error")}
              >
                <div className="flex items-center gap-2">
                  <NeedsTooltip reason={locked}>
                    <Input
                      id={controlId("graceHours")}
                      type="number"
                      inputMode="numeric"
                      min={GRACE_MIN}
                      max={GRACE_MAX}
                      step={1}
                      value={view.graceHours.value}
                      readOnly={!canEdit}
                      aria-invalid={!graceValid || undefined}
                      aria-describedby={`${rowIds(`${id}-grace-row`).helper} ${rowIds(`${id}-grace-row`).error}`}
                      className="w-28 max-md:h-11"
                      onChange={(e) => edit("graceHours", e.target.value)}
                    />
                  </NeedsTooltip>
                  <span className="text-sm text-muted-foreground">
                    {t("janitorPolicy.rows.grace.suffix")}
                  </span>
                </div>
              </PolicyRow>

              <PolicyRow
                id={`${id}-networks-row`}
                label={networksLabel}
                helper={t("janitorPolicy.rows.networks.helper")}
                overridden={view.removeUnusedNetworks.overridden}
                field={networksLabel}
                onReset={() => reset("removeUnusedNetworks")}
                locked={locked}
              >
                <NeedsTooltip reason={locked}>
                  <Switch
                    id={controlId("removeUnusedNetworks")}
                    checked={view.removeUnusedNetworks.value}
                    aria-labelledby={rowIds(`${id}-networks-row`).title}
                    aria-describedby={rowIds(`${id}-networks-row`).helper}
                    onCheckedChange={(on) => edit("removeUnusedNetworks", on)}
                  />
                </NeedsTooltip>
              </PolicyRow>

              <PolicyRow
                id={`${id}-containers-row`}
                label={containersLabel}
                helper={t("janitorPolicy.rows.containers.helper")}
                overridden={view.removeStoppedContainers.overridden}
                field={containersLabel}
                onReset={() => reset("removeStoppedContainers")}
                locked={locked}
              >
                <NeedsTooltip reason={locked}>
                  <Switch
                    id={controlId("removeStoppedContainers")}
                    checked={view.removeStoppedContainers.value}
                    aria-labelledby={rowIds(`${id}-containers-row`).title}
                    aria-describedby={rowIds(`${id}-containers-row`).helper}
                    onCheckedChange={(on) => edit("removeStoppedContainers", on)}
                  />
                </NeedsTooltip>
              </PolicyRow>
            </div>
          )}

          {firstRunDone === undefined ? null : (
            <RulesNote firstRunDone={firstRunDone} keep={keep} graceHours={grace} />
          )}

          {swarm && policy.data ? (
            <NodeOverridesTable
              envId={envId}
              envName={envName}
              nodes={policy.data.nodes}
              envKeepN={policy.data.keepN.value}
              canEdit={canEdit}
              locked={locked}
            />
          ) : null}

          {data && view ? (
            <div className="flex flex-col gap-3">
              {failure?.kind === "conflict" ? (
                <div role="alert" className="flex flex-wrap items-center gap-x-3 gap-y-2 text-sm">
                  <p className="text-destructive">
                    {failure.by
                      ? t("janitorPolicy.conflict.message", { user: failure.by })
                      : t("janitorPolicy.conflict.messageAnon")}
                  </p>
                  <Button
                    type="button"
                    variant="outline"
                    className="max-md:h-11 max-md:w-full"
                    onClick={reload}
                  >
                    {t("janitorPolicy.conflict.reload")}
                  </Button>
                </div>
              ) : failure?.kind === "failed" ? (
                <p role="alert" className="text-sm text-destructive">
                  {t("janitorPolicy.footer.saveFailed")} {t("janitorPolicy.footer.retry")}
                </p>
              ) : null}
              <div className="flex flex-wrap items-center gap-3">
                {canEdit ? (
                  footerButton
                ) : (
                  <NeedsTooltip reason={locked}>{footerButton}</NeedsTooltip>
                )}
                <p role="status" className="text-xs text-muted-foreground">
                  {dirty ? t("janitorPolicy.footer.unsaved") : ""}
                </p>
              </div>
            </div>
          ) : null}
        </CardContent>
      </Card>

      {reviewing && view ? (
        <CleanupPreviewSheet
          mode="policy"
          envId={envId}
          envName={envName}
          swarm={swarm}
          nodeCount={nodeCount}
          policy={fieldsOf(view)}
          onSavePolicy={saveNow}
          onOpenChange={setReviewing}
        />
      ) : null}

      <AlertDialog
        open={blocker.status === "blocked"}
        onOpenChange={(next) => {
          if (!next) blocker.reset?.();
        }}
      >
        <AlertDialogContent className="sm:max-w-120">
          <AlertDialogHeader>
            <AlertDialogTitle>{t("janitorPolicy.guard.title")}</AlertDialogTitle>
            <AlertDialogDescription>{t("janitorPolicy.guard.body")}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel autoFocus>{t("janitorPolicy.guard.keep")}</AlertDialogCancel>
            <AlertDialogAction onClick={() => blocker.proceed?.()}>
              {t("janitorPolicy.guard.discard")}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </section>
  );
}
