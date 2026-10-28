// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { zodResolver } from "@hookform/resolvers/zod";
import { useQuery } from "@tanstack/react-query";
import { Link, useNavigate } from "@tanstack/react-router";
import { useEffect, useId, useMemo, useRef, useState } from "react";
import { Controller, useForm } from "react-hook-form";

import { environmentsQuery, summaryQuery } from "@/api/environments";
import {
  checkStackName,
  type StackNameCheck,
  type StackRequestError,
  useCreateStack,
} from "@/api/stacks";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Field, FieldDescription, FieldError, FieldGroup, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { RadioGroup, RadioGroupItem } from "@/components/ui/radio-group";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Spinner } from "@/components/ui/spinner";
import { TruncatedText } from "@/features/resources/resource-table";
import { hasPermission, useMe } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { formatBytes } from "@/lib/format";
import { toastSuccess } from "@/lib/toast";
import { z } from "@/lib/zod-config";

// A stack name is also its swarm namespace or compose project, so it follows the server's rule for the
// target of the environment it is made on, the same rule that decides whether a stack made elsewhere can be
// adopted (D-48, D-53). On a swarm: letters in either case, numbers, dash and underscore, starting with a
// letter or number, at most 61 characters (no dot: swarm refuses one in a service or network name). On a
// standalone host: compose-go's project name, lowercase only, at most 255 characters. While no target is
// known (the All stacks list before an environment is chosen) the union of the two applies and the server's
// check decides.
interface NameRule {
  pattern: RegExp;
  max: number;
  // The i18n keys of the error and of the helper line.
  error: string;
  helper: string;
}

const SWARM_RULE: NameRule = {
  pattern: /^[A-Za-z0-9][A-Za-z0-9_-]*$/,
  max: 61,
  error: "stacks.create.errors.nameInvalidSwarm",
  helper: "stacks.create.name.helperSwarm",
};
const STANDALONE_RULE: NameRule = {
  pattern: /^[a-z0-9][a-z0-9_-]*$/,
  max: 255,
  error: "stacks.create.errors.nameInvalidStandalone",
  helper: "stacks.create.name.helperStandalone",
};
const UNION_RULE: NameRule = {
  pattern: /^[A-Za-z0-9][A-Za-z0-9_-]*$/,
  max: 255,
  error: "stacks.create.errors.nameInvalid",
  helper: "stacks.create.name.helper",
};

// ruleFor is the name rule of a target: true for a swarm, false for a standalone host, undefined while unknown.
function ruleFor(swarm: boolean | undefined): NameRule {
  if (swarm === undefined) return UNION_RULE;
  return swarm ? SWARM_RULE : STANDALONE_RULE;
}

function nameFits(rule: NameRule, name: string): boolean {
  return name.length <= rule.max && rule.pattern.test(name);
}

// The largest compose file the server takes (1 MiB). A bigger one is refused here, before it is read.
const MAX_FILE_BYTES = 1024 * 1024;

// Messages are i18n keys, never literals (D-65).
function schemaFor(needsEnvironment: boolean, rule: NameRule) {
  return z.object({
    env: needsEnvironment ? z.string().min(1, "stackLists.create.errors.envRequired") : z.string(),
    name: z
      .string()
      .min(1, "stacks.create.errors.nameRequired")
      .max(rule.max, rule.error)
      .regex(rule.pattern, rule.error),
    start: z.enum(["example", "empty", "upload"]),
  });
}
type Values = z.infer<ReturnType<typeof schemaFor>>;

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

interface CreateStackDialogProps {
  // The environment of an environment's own list. Left out, the dialog is the All stacks list's and
  // asks for the environment, among those the user may create stacks on.
  envId?: string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

// CreateStackDialog makes a stack with its first draft and takes the person to its page. Nothing
// is sent to any engine: the stack is Not deployed until it is reviewed and deployed. The name is
// checked when its field loses focus and again on submit; every refusal stays inside the dialog,
// which closes only when the stack was made.
export function CreateStackDialog({ envId, open, onOpenChange }: CreateStackDialogProps) {
  const t = useT();
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[90svh] overflow-y-auto sm:max-w-140">
        <DialogHeader>
          <DialogTitle>{t("stacks.create.title")}</DialogTitle>
          <DialogDescription>{t("stacks.create.body")}</DialogDescription>
        </DialogHeader>
        <CreateStackForm fixedEnvId={envId} onCancel={() => onOpenChange(false)} />
      </DialogContent>
    </Dialog>
  );
}

// A file the person chose: its name and size for the row, its text for the request.
interface Upload {
  name: string;
  size: number;
  text: string;
}

// A refusal of the whole request, shown inside the dialog with role=alert. A foreign stack's name
// carries the spec's second line and, for a user who can read stacks there, the way to the stack that is there.
interface Refusal {
  text: string;
  hint?: string;
  // The stack, not managed by Bayplan, that holds the name: its environment and its name.
  existing?: { envId: string; name: string };
}

// readText reads a file as UTF-8 text, and refuses what is not: a binary file is not a compose file.
function readText(file: File): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onerror = () => reject(reader.error ?? new Error("unreadable"));
    reader.onload = () => {
      try {
        resolve(new TextDecoder("utf-8", { fatal: true }).decode(reader.result as ArrayBuffer));
      } catch (e) {
        reject(e);
      }
    };
    reader.readAsArrayBuffer(file);
  });
}

// useIsSwarm says whether an environment is a swarm, from the list the sidebar has loaded or, for the
// local socket, the summary. It is undefined while no environment is chosen or its kind is not known yet.
function useIsSwarm(envId: string | undefined): boolean | undefined {
  const { data: environments } = useQuery(environmentsQuery);
  const { data: summary } = useQuery({ ...summaryQuery(envId ?? ""), enabled: Boolean(envId) });
  if (!envId) return undefined;
  const listed = environments?.items?.find((e) => e.id === envId);
  const type = summary?.environment.type;
  const kind =
    listed?.envType ?? (type ? (type === "standalone" ? "standalone" : "swarm") : undefined);
  return kind === undefined ? undefined : kind === "swarm";
}

function CreateStackForm({
  fixedEnvId,
  onCancel,
}: {
  fixedEnvId: string | undefined;
  onCancel: () => void;
}) {
  const t = useT();
  const ids = useId();
  const navigate = useNavigate();
  const create = useCreateStack();
  const { data: me } = useMe();
  const { data: environments } = useQuery(environmentsQuery);
  const needsEnvironment = fixedEnvId === undefined;

  // The environments the user may create stacks on: the choice of the All stacks list's dialog. One
  // alone is chosen for them.
  const options = useMemo(
    () =>
      (environments?.items ?? [])
        .filter((e) => hasPermission(me, "stacks.create", e.id))
        .map((e) => ({ id: e.id, name: e.name })),
    [environments, me],
  );

  // The target decides the name rule. The environment is the fixed one or, on the All stacks list, the one
  // chosen in the select. The form is declared first (the select's value is the form's), so the rule it
  // validates with is read through a ref that is set on every render.
  const ruleRef = useRef<NameRule>(UNION_RULE);
  const form = useForm<Values>({
    resolver: (values, context, options) =>
      zodResolver(schemaFor(needsEnvironment, ruleRef.current))(values, context, options),
    defaultValues: { env: "", name: "", start: "example" },
    mode: "onSubmit",
  });
  const fixedName = environments?.items?.find((e) => e.id === fixedEnvId)?.name ?? "";
  const chosenEnv = fixedEnvId ?? form.watch("env");
  const swarm = useIsSwarm(chosenEnv || undefined);
  const rule = ruleFor(swarm);
  ruleRef.current = rule;
  const start = form.watch("start");

  // A name that is already filled is judged again with the rule of the target when the target changes.
  // biome-ignore lint/correctness/useExhaustiveDependencies: only a change of the target runs it
  useEffect(() => {
    if (form.getValues("name")) void form.trigger("name");
  }, [swarm]);

  useEffect(() => {
    if (needsEnvironment && options.length === 1 && !form.getValues("env")) {
      form.setValue("env", options[0]?.id ?? "");
    }
  }, [needsEnvironment, options, form]);

  const [refusal, setRefusal] = useState<Refusal | null>(null);
  const [checking, setChecking] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [upload, setUpload] = useState<Upload | null>(null);
  const [fileError, setFileError] = useState<string | null>(null);
  const fileInput = useRef<HTMLInputElement>(null);
  // The check that is current: an answer to an earlier one is dropped.
  const checkNo = useRef(0);

  const keyOf = (field: keyof Values) => {
    const message = form.formState.errors[field]?.message;
    return typeof message === "string" ? message : undefined;
  };
  const nameKey = keyOf("name");
  const nameError = nameKey ? t(nameKey) : undefined;
  const envKey = keyOf("env");
  const envError = envKey ? t(envKey) : undefined;
  const target = useMemo(
    () => (swarm ? t("stacks.target.swarm") : t("stacks.target.standalone")),
    [swarm, t],
  );

  // show puts a verdict of the name check in the dialog and says whether the name can be used.
  function show(verdict: StackNameCheck, name: string, env: string): boolean {
    switch (verdict.status) {
      case "available":
        setRefusal(null);
        return true;
      case "invalid":
        form.setError("name", { type: "server", message: ruleRef.current.error });
        return false;
      case "clash":
        setRefusal({
          text: t("stackLists.create.errors.nameClash", { name }),
          hint: t("stackLists.create.errors.clashHint"),
          existing: { envId: env, name },
        });
        return false;
      default:
        setRefusal({ text: verdict.message });
        return false;
    }
  }

  // check asks the server about a name on an environment. A name the rule refuses is not asked about:
  // the field says so. A check that cannot be answered is null, and the create decides again.
  async function check(env: string, name: string, show_: boolean): Promise<StackNameCheck | null> {
    if (!env || !nameFits(ruleRef.current, name)) return null;
    const no = ++checkNo.current;
    if (show_) setChecking(true);
    try {
      const verdict = await checkStackName(env, name);
      if (no !== checkNo.current) return null;
      return verdict;
    } catch {
      return null;
    } finally {
      if (no === checkNo.current) setChecking(false);
    }
  }

  async function checkOnBlur() {
    const name = form.getValues("name");
    if (!nameFits(ruleRef.current, name)) {
      // Say what is wrong with it now, rather than at the submit.
      if (name) await form.trigger("name");
      return;
    }
    const env = fixedEnvId ?? form.getValues("env");
    const verdict = await check(env, name, true);
    if (verdict) show(verdict, name, env);
  }

  function refusalOf(err: StackRequestError, name: string, env: string): Refusal | null {
    switch (err.type) {
      case "stack_name_invalid":
        form.setError("name", { type: "server", message: ruleRef.current.error });
        form.setFocus("name");
        return null;
      case "stack_name_taken":
        return {
          text: t("stacks.create.errors.nameTaken", {
            name,
            env: options.find((o) => o.id === env)?.name ?? fixedName,
          }),
        };
      case "stack_name_clash":
        return {
          text: t("stackLists.create.errors.nameClash", { name }),
          hint: t("stackLists.create.errors.clashHint"),
          existing: { envId: env, name },
        };
      case "file_too_large":
        return { text: t("stackLists.create.errors.fileTooLarge") };
    }
    if (err.status === 0) return { text: t("stacks.create.errors.lost") };
    if (err.status === 403) return { text: t("stacks.create.errors.permission") };
    return { text: t("stacks.create.errors.generic") };
  }

  async function submit(values: Values) {
    if (submitting || create.isPending) return;
    if (values.start === "upload" && !upload) return;
    const env = fixedEnvId ?? values.env;
    setRefusal(null);
    setSubmitting(true);
    // The name is checked again: it may have been taken since the field lost focus.
    const verdict = await check(env, values.name, false);
    if (verdict && !show(verdict, values.name, env)) {
      setSubmitting(false);
      return;
    }
    create.mutate(
      {
        envId: env,
        body:
          values.start === "upload" && upload
            ? { name: values.name, start: "upload", compose: upload.text }
            : { name: values.name, start: values.start },
      },
      {
        onSuccess: (made) => {
          toastSuccess(t("stacks.create.success", { name: made.name }));
          void navigate({
            to: "/env/$envId/stacks/$stackId",
            params: { envId: env, stackId: made.id },
          });
        },
        onError: (err) => {
          setSubmitting(false);
          setRefusal(refusalOf(err, values.name, env));
        },
      },
    );
  }

  async function chooseFile(file: File | undefined) {
    if (!file) return;
    setFileError(null);
    if (file.size > MAX_FILE_BYTES) {
      setUpload(null);
      setFileError(t("stackLists.create.errors.fileTooLarge"));
      return;
    }
    try {
      const text = await readText(file);
      setUpload({ name: file.name, size: file.size, text });
    } catch {
      setUpload(null);
      setFileError(t("stackLists.create.errors.fileUnreadable"));
    }
  }

  function removeFile() {
    setUpload(null);
    setFileError(null);
    if (fileInput.current) fileInput.current.value = "";
  }

  const pending = submitting || create.isPending;
  const noFile = start === "upload" && !upload;
  return (
    <form noValidate onSubmit={form.handleSubmit(submit)} className="flex flex-col gap-4">
      {needsEnvironment ? null : (
        <p className="text-sm text-muted-foreground">
          {t("stacks.create.target", { name: fixedName, target })}
        </p>
      )}
      {refusal ? (
        <Alert variant="destructive" role="alert">
          <AlertDescription className="flex flex-col gap-1">
            <span>{refusal.text}</span>
            {refusal.hint ? (
              <span className="text-muted-foreground">
                {refusal.hint}
                {refusal.existing && hasPermission(me, "stacks.read", refusal.existing.envId) ? (
                  <>
                    {" "}
                    <Link
                      to="/env/$envId/unmanaged-stacks/$name"
                      params={{ envId: refusal.existing.envId, name: refusal.existing.name }}
                      className="rounded-sm text-accent-ink underline underline-offset-3 hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
                    >
                      {t("stackLists.create.errors.openClash", { name: refusal.existing.name })}
                    </Link>
                  </>
                ) : null}
              </span>
            ) : null}
          </AlertDescription>
        </Alert>
      ) : null}
      <fieldset disabled={pending} className="contents">
        <FieldGroup className="gap-4">
          {needsEnvironment ? (
            <Field data-invalid={envError ? true : undefined}>
              <FieldLabel htmlFor={`${ids}-env`}>
                {t("stackLists.create.environment.label")}
              </FieldLabel>
              <Controller
                control={form.control}
                name="env"
                render={({ field }) => (
                  <Select
                    value={field.value}
                    onValueChange={(next) => {
                      field.onChange(next);
                      form.clearErrors("env");
                      setRefusal(null);
                      // The same name may be free on one environment and taken on another.
                      const name = form.getValues("name");
                      void check(next, name, true).then((verdict) => {
                        if (verdict) show(verdict, name, next);
                      });
                    }}
                  >
                    <SelectTrigger
                      id={`${ids}-env`}
                      className={`w-full ${TOUCH_HEIGHT}`}
                      aria-invalid={Boolean(envError)}
                    >
                      <SelectValue placeholder={t("stackLists.create.environment.placeholder")} />
                    </SelectTrigger>
                    <SelectContent>
                      {options.map((o) => (
                        <SelectItem key={o.id} value={o.id}>
                          {o.name}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                )}
              />
              {envError ? <FieldError>{envError}</FieldError> : null}
            </Field>
          ) : null}

          <Field data-invalid={nameError ? true : undefined}>
            <FieldLabel htmlFor={`${ids}-name`}>{t("stacks.create.name.label")}</FieldLabel>
            <div className="relative">
              <Input
                id={`${ids}-name`}
                autoComplete="off"
                autoCapitalize="off"
                autoCorrect="off"
                spellCheck={false}
                maxLength={rule.max}
                aria-invalid={Boolean(nameError)}
                aria-describedby={`${ids}-name-helper`}
                className={`${TOUCH_HEIGHT} ${checking ? "pe-9" : ""}`}
                {...form.register("name", {
                  onChange: () => {
                    // A name that was refused is a new name now.
                    checkNo.current++;
                    setChecking(false);
                    if (form.formState.errors.name) form.clearErrors("name");
                    setRefusal(null);
                  },
                  onBlur: () => void checkOnBlur(),
                })}
              />
              {checking ? (
                <Spinner
                  aria-label={t("stackLists.create.checking")}
                  className="absolute end-2.5 top-1/2 -translate-y-1/2 text-muted-foreground max-md:top-3.5 max-md:translate-y-0"
                />
              ) : null}
            </div>
            <FieldDescription id={`${ids}-name-helper`}>{t(rule.helper)}</FieldDescription>
            {nameError ? <FieldError>{nameError}</FieldError> : null}
          </Field>

          <fieldset className="flex flex-col gap-2">
            <legend className="mb-1 text-sm font-medium">{t("stacks.create.start.label")}</legend>
            <Controller
              control={form.control}
              name="start"
              render={({ field }) => (
                <RadioGroup
                  value={field.value}
                  onValueChange={(next) => {
                    field.onChange(next);
                    setFileError(null);
                  }}
                  aria-label={t("stacks.create.start.label")}
                >
                  {(["example", "empty", "upload"] as const).map((value) => (
                    <div key={value} className="flex items-center gap-2 max-md:min-h-11">
                      <RadioGroupItem value={value} id={`${ids}-start-${value}`} />
                      <Label htmlFor={`${ids}-start-${value}`}>
                        {value === "upload"
                          ? t("stackLists.create.start.upload")
                          : t(`stacks.create.start.${value}`)}
                      </Label>
                    </div>
                  ))}
                </RadioGroup>
              )}
            />
            {start === "upload" ? (
              <div className="ms-6 flex flex-col gap-2">
                {/* The system's file chooser opens from the button; the input is for assistive technology and tests. */}
                <input
                  ref={fileInput}
                  type="file"
                  tabIndex={-1}
                  aria-label={t("stackLists.create.start.fileInput")}
                  className="sr-only"
                  onChange={(e) => void chooseFile(e.target.files?.[0])}
                />
                {upload ? (
                  <div className="flex min-w-0 flex-wrap items-center gap-2">
                    <span className="flex min-w-0 items-baseline gap-1">
                      <TruncatedText className="max-w-56">{upload.name}</TruncatedText>
                      <span className="shrink-0 text-sm text-muted-foreground">
                        {`(${formatBytes(upload.size)})`}
                      </span>
                    </span>
                    <Button
                      type="button"
                      variant="ghost"
                      size="sm"
                      className="max-md:h-11"
                      onClick={removeFile}
                    >
                      {t("stackLists.create.start.removeFile")}
                    </Button>
                  </div>
                ) : (
                  <Button
                    type="button"
                    variant="outline"
                    className="w-fit max-md:h-11"
                    onClick={() => fileInput.current?.click()}
                  >
                    {t("stackLists.create.start.chooseFile")}
                  </Button>
                )}
                {fileError ? (
                  <Alert variant="destructive" role="alert">
                    <AlertDescription>{fileError}</AlertDescription>
                  </Alert>
                ) : null}
              </div>
            ) : null}
          </fieldset>
        </FieldGroup>
      </fieldset>
      <DialogFooter>
        <Button type="button" variant="outline" className="max-md:h-11" onClick={onCancel}>
          {t("stacks.create.cancel")}
        </Button>
        <NeedsTooltip reason={noFile ? t("stackLists.create.chooseFileReason") : undefined}>
          <Button type="submit" className="max-md:h-11" disabled={pending}>
            {pending ? <Spinner aria-hidden="true" /> : null}
            {pending ? t("stacks.create.pending") : t("stacks.create.submit")}
          </Button>
        </NeedsTooltip>
      </DialogFooter>
    </form>
  );
}
