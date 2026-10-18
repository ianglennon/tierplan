// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { Check, ChevronDown, ChevronRight, CircleX, Minus } from "lucide-react";
import { useId, useRef, useState } from "react";

import { configRotatePreviewQuery, isConfigGone, useRotateConfig } from "@/api/configs";
import { isSecretGone, secretRotatePreviewQuery, useRotateSecret } from "@/api/secrets";
import { ServiceActionError } from "@/api/services";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Field, FieldDescription, FieldError, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { NAME_PATTERN } from "@/features/secrets/object-sheet";
import {
  EMPTY_VALUE,
  MAX_KB,
  type ObjectKind,
  type ObjectValue,
  toBase64,
  type ValueProblem,
  ValueSourceField,
  valueBytes,
  valueProblem,
} from "@/features/secrets/value-source-field";
import { RolloutProgress } from "@/features/services/rollout-progress";
import { SpecDiff } from "@/features/spec-editor/spec-diff";
import { useT } from "@/i18n/use-t";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

type Templating = "none" | "golang";
type Step = "form" | "review" | "run";
type StepState = "waiting" | "working" | "done" | "failed" | "kept";

// The reasons a stopped run has words for; anything else is said generally.
const STOPPED_TYPES = new Set([
  "service_changed",
  "privileged_service",
  "spec_rejected",
  "image_unresolved",
  "service_not_found",
  "stack_owned",
]);

// What a rotate came to, as the dialog draws it.
type Outcome =
  | {
      status: "done";
      services: Updated[];
      skipped: number;
      oldRemoved: boolean;
      removeCode: string;
      updated: number;
      newName: string;
    }
  | { status: "error"; error: ServiceActionError };

interface Updated {
  serviceId: string;
  name: string;
  state: string;
}

export interface RotateTarget {
  id: string;
  name: string;
}

interface RotateDialogProps {
  // Whether a secret or a config is rotated: they share the dialog, the words and the flow.
  kind: ObjectKind;
  envId: string;
  // The object to give a new version, or null while the dialog is closed.
  object: RotateTarget | null;
  onClose: () => void;
}

// RotateDialog is "New version of {name}" (D-26, UI-SPEC "Secrets and configs"): step 1 asks for the
// new name, the value and whether to remove the old object; step 2 lists every service that uses the
// object with what would change in each, and the stack-owned ones as skipped; then the server creates
// the new object, moves the services one at a time and removes the old one once nothing uses it, and
// the dialog follows it step by step. The value lives in this component's state until it is sent,
// once, as base64; it is cleared when the dialog closes and when the request is answered, and it is
// in no toast, storage, address or cache. Nothing closes the dialog while the request is out.
export function RotateDialog({ kind, envId, object, onClose }: RotateDialogProps) {
  const busy = useRef(false);
  return (
    <Dialog
      open={object !== null}
      onOpenChange={(open) => {
        if (!open && !busy.current) onClose();
      }}
    >
      <DialogContent className="max-h-[90svh] grid-rows-[auto_minmax(0,1fr)_auto] sm:max-w-200">
        {object ? (
          <RotateBody
            key={object.id}
            kind={kind}
            envId={envId}
            object={object}
            busy={busy}
            onClose={onClose}
          />
        ) : null}
      </DialogContent>
    </Dialog>
  );
}

function previewOptions(kind: ObjectKind, envId: string, id: string, newName: string) {
  return kind === "config"
    ? configRotatePreviewQuery(envId, id, newName)
    : secretRotatePreviewQuery(envId, id, newName);
}

function StepMark({ state }: { state: StepState }) {
  switch (state) {
    case "working":
      return <Spinner aria-hidden="true" />;
    case "done":
      return <Check aria-hidden="true" className="size-4 text-status-online-text" />;
    case "failed":
      return <CircleX aria-hidden="true" className="size-4 text-destructive" />;
    default:
      return <Minus aria-hidden="true" className="size-4 text-muted-foreground" />;
  }
}

interface BodyProps {
  kind: ObjectKind;
  envId: string;
  object: RotateTarget;
  busy: React.RefObject<boolean>;
  onClose: () => void;
}

function RotateBody({ kind, envId, object, busy, onClose }: BodyProps) {
  const t = useT();
  const ids = useId();
  const navigate = useNavigate();
  // The words are the secrets' or the configs'; the flow is the same.
  const ns = kind === "config" ? "configs" : "secrets";
  const kindWord = t(`secrets.rotate.kind.${kind}`);
  const secretRun = useRotateSecret(envId);
  const configRun = useRotateConfig(envId);
  const run = kind === "config" ? configRun : secretRun;

  const [step, setStep] = useState<Step>("form");
  // Null until the person edits the name: it then follows the name the server proposes.
  const [typedName, setTypedName] = useState<string | null>(null);
  const [value, setValue] = useState<ObjectValue>(EMPTY_VALUE);
  const [typedTemplating, setTypedTemplating] = useState<Templating | null>(null);
  const [removeOld, setRemoveOld] = useState(true);
  const [submitted, setSubmitted] = useState(false);
  const [expanded, setExpanded] = useState<ReadonlySet<string>>(new Set());
  const [outcome, setOutcome] = useState<Outcome | null>(null);
  // What was asked, kept for the words of the result after the form has gone.
  const [asked, setAsked] = useState({ newName: "", removeOld: true });

  // The first read, with the name the server proposes, is what step 1 prefills from.
  const base = useQuery(previewOptions(kind, envId, object.id, ""));
  const proposed = base.data?.newName ?? "";
  const newName = (typedName ?? proposed).trim();
  // Once the name was edited the services are read again under it, so each diff names it.
  const reread = step !== "form" && newName !== "" && newName !== proposed;
  const typed = useQuery({
    ...previewOptions(kind, envId, object.id, newName),
    enabled: reread,
  });
  const preview = reread ? typed : base;
  const templating: Templating =
    typedTemplating ?? (base.data?.templating === "golang" ? "golang" : "none");

  const nameProblem: "required" | "invalid" | undefined =
    newName === "" ? "required" : NAME_PATTERN.test(newName) ? undefined : "invalid";
  const problem: ValueProblem | undefined = valueProblem(kind, value);
  const gone = kind === "config" ? isConfigGone(base.error) : isSecretGone(base.error);

  function toReview() {
    setSubmitted(true);
    if (nameProblem || problem || !base.data) return;
    setStep("review");
  }

  async function submit() {
    const bytes = valueBytes(value);
    if (busy.current || bytes === null || !preview.data || preview.data.nameTaken) return;
    busy.current = true;
    setAsked({ newName, removeOld });
    setStep("run");
    try {
      const data = toBase64(bytes);
      const result =
        kind === "config"
          ? await configRun.mutateAsync({
              id: object.id,
              body: {
                name: newName,
                data,
                removeOld,
                ...(templating === "golang" ? { templating } : {}),
              },
            })
          : await secretRun.mutateAsync({
              id: object.id,
              body: { name: newName, data, removeOld },
            });
      // The value has been sent and kept: nothing needs it any more.
      setValue(EMPTY_VALUE);
      setOutcome({
        status: "done",
        services: (result.services ?? []).map((s) => ({
          serviceId: s.serviceId,
          name: s.service,
          state: s.state,
        })),
        skipped: result.skipped,
        updated: result.updated,
        oldRemoved: result.oldRemoved,
        removeCode: result.removeCode,
        newName: result.name,
      });
    } catch (e) {
      setOutcome({
        status: "error",
        error: e instanceof ServiceActionError ? e : new ServiceActionError(0),
      });
    } finally {
      busy.current = false;
    }
  }

  function toggle(id: string) {
    setExpanded((prev) => {
      const next = new Set(prev);
      if (!next.delete(id)) next.add(id);
      return next;
    });
  }

  function service(serviceId: string) {
    onClose();
    void navigate({ to: "/env/$envId/services/$serviceId", params: { envId, serviceId } });
  }

  const pending = run.isPending;
  const users = preview.data?.users ?? [];
  const standalone = users.filter((u) => !u.skipped);
  const stacked = users.length - standalone.length;
  const title = t("secrets.rotate.title", { name: object.name });
  const stepNumber = step === "form" ? 1 : 2;

  return (
    <>
      <DialogHeader>
        <DialogTitle className="break-words">{title}</DialogTitle>
        <DialogDescription>
          {t("secrets.rotate.body", { kind: kindWord, name: object.name })}
        </DialogDescription>
        {step !== "run" ? (
          <p className="text-xs text-muted-foreground">
            {t("secrets.rotate.step", { n: stepNumber })}
          </p>
        ) : null}
      </DialogHeader>

      <div className="flex min-h-0 flex-col gap-4 overflow-y-auto">
        {/* Step 1 stays mounted behind the later steps: the value field hands its owner an empty value when
            it goes away, and going back to step 1 keeps what was typed. */}
        <form
          noValidate
          hidden={step !== "form"}
          className="flex flex-col gap-6"
          onSubmit={(e) => {
            e.preventDefault();
            toReview();
          }}
        >
          {gone ? (
            <Alert variant="destructive" role="alert">
              <AlertDescription>{t("secrets.rotate.gone", { kind: kindWord })}</AlertDescription>
            </Alert>
          ) : base.isError ? (
            <Alert variant="destructive" role="alert">
              <AlertDescription className="flex flex-wrap items-center gap-3">
                <span>{t("secrets.rotate.loadFailed", { name: object.name })}</span>
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  className="max-md:h-11"
                  onClick={() => void base.refetch()}
                >
                  {t("secrets.rotate.retry")}
                </Button>
              </AlertDescription>
            </Alert>
          ) : null}
          <Field data-invalid={submitted && nameProblem ? true : undefined}>
            <FieldLabel htmlFor={`${ids}-name`}>{t("secrets.rotate.name.label")}</FieldLabel>
            <Input
              id={`${ids}-name`}
              value={typedName ?? proposed}
              maxLength={64}
              autoComplete="off"
              autoCapitalize="off"
              autoCorrect="off"
              spellCheck={false}
              aria-invalid={submitted && nameProblem ? true : undefined}
              aria-describedby={`${ids}-name-helper${submitted && nameProblem ? ` ${ids}-name-error` : ""}`}
              className={`font-mono ${TOUCH_HEIGHT}`}
              onChange={(e) => setTypedName(e.target.value)}
            />
            <FieldDescription id={`${ids}-name-helper`}>
              {t("secrets.rotate.name.helper")}
            </FieldDescription>
            {submitted && nameProblem ? (
              <FieldError id={`${ids}-name-error`}>
                {t(
                  nameProblem === "required"
                    ? `${ns}.errors.nameRequired`
                    : `${ns}.errors.nameInvalid`,
                )}
              </FieldError>
            ) : null}
          </Field>

          <ValueSourceField
            kind={kind}
            value={value}
            problem={submitted ? problem : undefined}
            onChange={setValue}
          />

          {kind === "config" ? (
            <Field>
              <FieldLabel htmlFor={`${ids}-templating`}>
                {t("configs.sheet.templating.label")}
              </FieldLabel>
              <Select
                value={templating}
                onValueChange={(next) => setTypedTemplating(next === "golang" ? "golang" : "none")}
              >
                <SelectTrigger id={`${ids}-templating`} className="w-full max-md:h-11">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="none">{t("configs.sheet.templating.none")}</SelectItem>
                  <SelectItem value="golang">{t("configs.sheet.templating.golang")}</SelectItem>
                </SelectContent>
              </Select>
              <FieldDescription>{t("configs.sheet.templating.helper")}</FieldDescription>
            </Field>
          ) : null}

          <Field orientation="horizontal">
            <Checkbox
              id={`${ids}-remove`}
              checked={removeOld}
              onCheckedChange={(next) => setRemoveOld(next === true)}
            />
            <FieldLabel htmlFor={`${ids}-remove`} className="font-normal break-all">
              {t("secrets.rotate.removeOld", { name: object.name })}
            </FieldLabel>
          </Field>
        </form>

        {step === "review" ? (
          <section
            aria-label={t("secrets.rotate.table.caption", { name: object.name })}
            className="flex flex-col gap-4"
          >
            {preview.isError ? (
              <Alert variant="destructive" role="alert">
                <AlertDescription className="flex flex-wrap items-center gap-3">
                  <span>{t("secrets.rotate.loadFailed", { name: object.name })}</span>
                  <Button
                    type="button"
                    variant="outline"
                    size="sm"
                    className="max-md:h-11"
                    onClick={() => void preview.refetch()}
                  >
                    {t("secrets.rotate.retry")}
                  </Button>
                </AlertDescription>
              </Alert>
            ) : !preview.data ? (
              <div
                role="status"
                aria-busy="true"
                aria-label={t("secrets.rotate.loading")}
                className="flex flex-col gap-2 rounded-lg p-2 ring-1 ring-foreground/10"
              >
                {Array.from({ length: 3 }, (_, i) => (
                  // biome-ignore lint/suspicious/noArrayIndexKey: identical placeholders
                  <Skeleton key={i} className="h-9 w-full" />
                ))}
              </div>
            ) : (
              <>
                {preview.data.nameTaken ? (
                  <Alert variant="destructive" role="alert">
                    <AlertDescription>
                      {t("secrets.rotate.nameTaken", { kind: kindWord, name: newName })}
                    </AlertDescription>
                  </Alert>
                ) : null}
                {users.length === 0 ? (
                  <p className="text-sm">
                    {t("secrets.rotate.none", { kind: kindWord, name: object.name })}
                  </p>
                ) : (
                  <div className="rounded-lg ring-1 ring-foreground/10">
                    <Table>
                      <caption className="sr-only">
                        {t("secrets.rotate.table.caption", { name: object.name })}
                      </caption>
                      <TableHeader>
                        <TableRow>
                          <TableHead scope="col">{t("secrets.rotate.table.service")}</TableHead>
                          <TableHead scope="col">{t("secrets.rotate.table.stack")}</TableHead>
                          <TableHead scope="col">{t("secrets.rotate.table.change")}</TableHead>
                        </TableRow>
                      </TableHeader>
                      <TableBody>
                        {users.flatMap((u) => {
                          const open = expanded.has(u.serviceId);
                          const rows = [
                            <TableRow key={u.serviceId}>
                              <TableCell className="align-top font-mono break-all whitespace-normal">
                                {u.service}
                              </TableCell>
                              <TableCell className="align-top break-all whitespace-normal">
                                {u.stack || t("secrets.rotate.table.none")}
                              </TableCell>
                              <TableCell className="align-top whitespace-normal">
                                {u.skipped ? (
                                  t("secrets.rotate.table.skipped", { stack: u.stack })
                                ) : (
                                  <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
                                    <span className="break-all">
                                      {t("secrets.rotate.table.willUse", { name: newName })}
                                    </span>
                                    <Button
                                      type="button"
                                      variant="ghost"
                                      size="sm"
                                      className="max-md:h-11"
                                      aria-expanded={open}
                                      aria-label={t(
                                        open
                                          ? "secrets.rotate.table.hide"
                                          : "secrets.rotate.table.show",
                                        { service: u.service },
                                      )}
                                      onClick={() => toggle(u.serviceId)}
                                    >
                                      {open ? (
                                        <ChevronDown aria-hidden="true" />
                                      ) : (
                                        <ChevronRight aria-hidden="true" />
                                      )}
                                    </Button>
                                  </div>
                                )}
                              </TableCell>
                            </TableRow>,
                          ];
                          if (open && !u.skipped) {
                            rows.push(
                              <TableRow key={`${u.serviceId}:diff`}>
                                <TableCell colSpan={3} className="whitespace-normal">
                                  <SpecDiff name={u.service} current={u.current} next={u.next} />
                                </TableCell>
                              </TableRow>,
                            );
                          }
                          return rows;
                        })}
                      </TableBody>
                    </Table>
                  </div>
                )}
                {stacked > 0 ? (
                  <Alert variant="default" role="status">
                    <AlertDescription>
                      {t("secrets.rotate.stacks", { count: stacked, name: object.name })}
                    </AlertDescription>
                  </Alert>
                ) : null}
              </>
            )}
          </section>
        ) : null}

        {step === "run" ? (
          <RunView
            t={t}
            kind={kind}
            ns={ns}
            object={object}
            asked={asked}
            outcome={outcome}
            pending={pending}
            names={standalone.map((u) => ({ id: u.serviceId, name: u.service }))}
            onService={service}
            envId={envId}
          />
        ) : null}
      </div>

      <DialogFooter className="max-sm:flex-col-reverse">
        {step === "form" ? (
          <>
            <Button type="button" variant="outline" className={TOUCH_HEIGHT} onClick={onClose}>
              {t("secrets.rotate.cancel")}
            </Button>
            <Button
              type="button"
              className={`${TOUCH_HEIGHT} aria-disabled:cursor-not-allowed aria-disabled:opacity-50`}
              aria-disabled={!base.data}
              onClick={toReview}
            >
              {t("secrets.rotate.review")}
            </Button>
          </>
        ) : null}
        {step === "review" ? (
          <>
            <Button
              type="button"
              variant="outline"
              className={TOUCH_HEIGHT}
              onClick={() => setStep("form")}
            >
              {t("secrets.rotate.back")}
            </Button>
            <Button
              type="button"
              className={`${TOUCH_HEIGHT} aria-disabled:cursor-not-allowed aria-disabled:opacity-50`}
              aria-disabled={!preview.data || preview.data.nameTaken}
              onClick={() => void submit()}
            >
              {t("secrets.rotate.submit")}
            </Button>
          </>
        ) : null}
        {step === "run" ? (
          <>
            {outcome?.status === "error" && !createdOf(outcome.error) ? (
              <Button
                type="button"
                variant="outline"
                className={TOUCH_HEIGHT}
                onClick={() => {
                  setOutcome(null);
                  run.reset();
                  setStep("form");
                }}
              >
                {t("secrets.rotate.back")}
              </Button>
            ) : null}
            <Button type="button" className={TOUCH_HEIGHT} disabled={pending} onClick={onClose}>
              {pending ? (
                <>
                  <Spinner aria-hidden="true" />
                  {t("secrets.rotate.pending")}
                </>
              ) : (
                t("secrets.rotate.close")
              )}
            </Button>
          </>
        ) : null}
      </DialogFooter>
    </>
  );
}

type Translate = (key: string, options?: Record<string, unknown>) => string;

// entriesAt lists the messages a refusal carries under a location.
function entriesAt(error: ServiceActionError, location: string): string[] {
  return error.entries.filter((e) => e.location === location).map((e) => e.message);
}

// createdOf is the name of the new object when the run got as far as making it: the refusal of a run
// that stopped part-way carries it, the refusal of a create does not.
function createdOf(error: ServiceActionError): string | undefined {
  return entriesAt(error, "created")[0];
}

interface RunViewProps {
  t: Translate;
  kind: ObjectKind;
  ns: "secrets" | "configs";
  envId: string;
  object: RotateTarget;
  asked: { newName: string; removeOld: boolean };
  outcome: Outcome | null;
  pending: boolean;
  // The standalone services the review listed, to put an ID to a name the server answered with.
  names: { id: string; name: string }[];
  onService: (serviceId: string) => void;
}

// RunView follows the run: the steps in order, each with its state as a word beside its mark, a
// rolling update under every service that moved, and the sentence for how it ended. The server does
// all of it in one request, so while it is out the first step is working and the rest wait.
function RunView({
  t,
  kind,
  ns,
  envId,
  object,
  asked,
  outcome,
  pending,
  names,
  onService,
}: RunViewProps) {
  const idOf = (name: string) => names.find((n) => n.name === name)?.id ?? "";
  const steps: {
    key: string;
    label: string;
    state: StepState;
    serviceId?: string;
    name?: string;
  }[] = [];
  let sentence: React.ReactNode = null;
  let reason: string | undefined;
  const create = (state: StepState) =>
    steps.push({
      key: "create",
      label: t("secrets.rotate.run.create", { name: asked.newName }),
      state,
    });
  const update = (name: string, serviceId: string, state: StepState) =>
    steps.push({
      key: `update:${name}`,
      label: t("secrets.rotate.run.update", { name }),
      state,
      serviceId,
      name,
    });
  const remove = (state: StepState) => {
    if (asked.removeOld)
      steps.push({
        key: "remove",
        label: t("secrets.rotate.run.remove", { name: object.name }),
        state,
      });
  };

  if (outcome === null || pending) {
    create("working");
    for (const n of names) update(n.name, n.id, "waiting");
    remove("waiting");
  } else if (outcome.status === "done") {
    create("done");
    for (const s of outcome.services) {
      if (s.state === "skipped") continue;
      update(s.name, s.serviceId, "done");
    }
    remove(outcome.oldRemoved ? "done" : "kept");
    sentence =
      outcome.updated === 0 ? (
        <p>{t("secrets.rotate.run.doneNone", { name: object.name, new: outcome.newName })}</p>
      ) : (
        <p>
          {t("secrets.rotate.run.done", {
            count: outcome.updated,
            name: object.name,
            new: outcome.newName,
          })}
        </p>
      );
    if (!outcome.oldRemoved && asked.removeOld && outcome.removeCode !== "") {
      reason =
        outcome.removeCode === "object_in_use"
          ? t("secrets.rotate.run.stays", { name: object.name, new: outcome.newName })
          : t("secrets.rotate.run.removeFailed", {
              name: object.name,
              where: t(kind === "config" ? "configs.list.title" : "secrets.list.title"),
            });
    }
  } else {
    const err = outcome.error;
    const made = createdOf(err);
    if (made === undefined) {
      create("failed");
      for (const n of names) update(n.name, n.id, "waiting");
      remove("waiting");
      sentence = (
        <p>
          {t("secrets.rotate.run.createFailed", {
            new: asked.newName,
            reason: createReason(t, ns, kind, err, asked.newName),
          })}
        </p>
      );
    } else {
      const updated = entriesAt(err, "updated");
      const failed = entriesAt(err, "failedAt")[0];
      const pendingNames = entriesAt(err, "pending");
      create("done");
      for (const n of updated) update(n, idOf(n), "done");
      if (failed !== undefined) update(failed, idOf(failed), "failed");
      for (const n of pendingNames) update(n, idOf(n), "waiting");
      remove("waiting");
      sentence = (
        <p>
          {t("secrets.rotate.run.partial", {
            updated: updated.length,
            count: updated.length + (failed === undefined ? 0 : 1) + pendingNames.length,
            name: object.name,
            new: made,
          })}
        </p>
      );
      reason = stoppedReason(t, err, failed ?? "");
    }
  }

  return (
    <section className="flex flex-col gap-4">
      <ol aria-label={t("secrets.rotate.run.label")} className="flex flex-col gap-3">
        {steps.map((s) => (
          <li key={s.key} className="flex flex-col gap-2">
            <div className="flex items-center gap-2 text-sm">
              <StepMark state={s.state} />
              <span className="min-w-0 font-mono break-all">{s.label}</span>
              <span className="text-muted-foreground">
                {t(`secrets.rotate.run.status.${s.state}`)}
              </span>
            </div>
            {s.state === "done" && s.serviceId ? (
              <RolloutProgress
                envId={envId}
                serviceId={s.serviceId}
                name={s.name ?? ""}
                onRollback={() => onService(s.serviceId ?? "")}
                onRetry={() => onService(s.serviceId ?? "")}
              />
            ) : null}
          </li>
        ))}
      </ol>
      {outcome ? (
        <Alert
          variant={outcome.status === "done" ? "default" : "destructive"}
          role={outcome.status === "done" ? "status" : "alert"}
        >
          <AlertDescription className="flex flex-col gap-2">
            {sentence}
            {reason ? <p>{reason}</p> : null}
          </AlertDescription>
        </Alert>
      ) : null}
    </section>
  );
}

// createReason words why the new object could not be made, from the problem type the server named. A
// type is never shown.
function createReason(
  t: Translate,
  ns: "secrets" | "configs",
  kind: ObjectKind,
  err: ServiceActionError,
  name: string,
): string {
  switch (err.type) {
    case "name_taken":
      return t(`${ns}.errors.nameTaken`, { name });
    case "name_invalid":
      return t(`${ns}.errors.nameInvalid`);
    case "value_required":
      return t("secrets.value.empty");
    case "value_too_large":
      return t("secrets.value.tooBig", { max: MAX_KB[kind] });
    case "templating_invalid":
      return t("configs.errors.templating");
    case "host_read_only":
      return t(`${ns}.errors.hostReadOnly`);
    case "no_manager_connected":
      return t(`${ns}.errors.noManager`);
    case "not_a_swarm":
      return t(`${ns}.errors.notASwarm`);
    case "secret_not_found":
    case "config_not_found":
      return t("secrets.rotate.gone", { kind: t(`secrets.rotate.kind.${kind}`) });
  }
  return err.status === 403 ? t(`${ns}.errors.permission`) : t(`${ns}.errors.generic`);
}

// stoppedReason words why a run stopped at a service.
function stoppedReason(t: Translate, err: ServiceActionError, service: string): string {
  const type = err.type ?? "";
  if (type === "host_read_only" || type === "no_manager_connected") {
    return t(`secrets.errors.${type === "host_read_only" ? "hostReadOnly" : "noManager"}`);
  }
  return t(`secrets.rotate.stopped.${STOPPED_TYPES.has(type) ? type : "generic"}`, { service });
}
