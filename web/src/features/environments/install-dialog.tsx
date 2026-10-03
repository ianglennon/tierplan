// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { zodResolver } from "@hookform/resolvers/zod";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { type RefObject, useEffect, useRef, useState } from "react";
import { useForm } from "react-hook-form";
import { z } from "zod";

import {
  createAgentEnvironment,
  environmentsQuery,
  type InstallInfo,
  InstallRequestError,
  rotateToken,
} from "@/api/environments";
import { CopyableCode } from "@/components/copyable-code";
import { TickedText } from "@/components/ticked-text";
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
import { Spinner } from "@/components/ui/spinner";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group";
import { WaitingPanel } from "@/features/environments/waiting-panel";
import { useT } from "@/i18n/use-t";

// The longest name the server accepts, counted in characters.
const NAME_MAX = 64;

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

type EnvKind = "swarm" | "standalone";
type View = "command" | "composeFile";

// Validation here is UX only: the server checks the name again. The message is
// an i18n key, never a literal (D-65). A character is counted once however it
// is encoded, so an emoji is one of the 64.
const nameSchema = z.object({
  name: z.string().refine((value) => Array.from(value.trim()).length <= NAME_MAX, {
    error: "install.name.tooLong",
  }),
});
type NameValues = z.input<typeof nameSchema>;

// Where the dialog is. Everything the join token touches lives in this value,
// which belongs to InstallFlow's state: it is gone when the dialog closes
// (D-20, T-02-53).
type Step =
  | { kind: "name" }
  | { kind: "confirm" }
  | {
      kind: "install";
      install: InstallInfo;
      envId: string;
      envName?: string;
      envType?: EnvKind;
      hasAgents: boolean;
    };

interface FlowProps {
  mode: "add" | "rotate";
  envId?: string;
  onClose: () => void;
  onBusyChange: (busy: boolean) => void;
  // Gets the element that should take focus when the dialog opens, if it is
  // not the first control.
  initialFocus: RefObject<HTMLElement | null>;
}

// failureKey picks the sentence for a failed call, as a key under
// install.errors. A missing address is the one failure an admin can fix from
// here, so it says so.
function failureKey(error: unknown, fallback: "create" | "rotate"): string {
  if (error instanceof InstallRequestError && error.publicUrlRequired) return "publicUrlRequired";
  return fallback;
}

interface ViewToggleProps {
  value: View;
  onChange: (view: View) => void;
}

function ViewToggle({ value, onChange }: ViewToggleProps) {
  const t = useT();
  return (
    <ToggleGroup
      type="single"
      variant="outline"
      size="sm"
      value={value}
      aria-label={t("install.view.label")}
      onValueChange={(next) => {
        // A pressed item cannot be pressed off: one of the two always shows.
        if (next === "command" || next === "composeFile") onChange(next);
      }}
    >
      <ToggleGroupItem value="command" className="max-md:h-11">
        {t("install.view.command")}
      </ToggleGroupItem>
      <ToggleGroupItem value="composeFile" className="max-md:h-11">
        {t("install.view.composeFile")}
      </ToggleGroupItem>
    </ToggleGroup>
  );
}

interface TabBodyProps {
  kind: EnvKind;
  info: InstallInfo;
  view: View;
  onViewChange: (view: View) => void;
  // True for a swarm that has agents, whose answer carries rotateCommand.
  update: boolean;
}

// TabBody is one tab: the Command or Compose file toggle, the masked block and
// the helper under it. A swarm that already has agents gets the update variant
// instead (UI-SPEC default 3): one command that swaps the secret.
function TabBody({ kind, info, view, onViewChange, update }: TabBodyProps) {
  const t = useT();

  if (kind === "swarm" && update) {
    return (
      <div className="flex flex-col gap-3">
        <h3 className="text-sm font-medium">{t("install.update.heading")}</h3>
        <p className="text-sm text-muted-foreground">{t("install.update.helper")}</p>
        <CopyableCode
          code={info.swarm.rotateCommand ?? ""}
          secret={info.joinToken}
          label={t("install.copyLabel.command")}
        />
        <p className="text-sm text-muted-foreground">
          <TickedText text={t("install.update.serviceName")} />
        </p>
      </div>
    );
  }

  const compose = view === "composeFile";
  let code = info.standalone.command;
  if (kind === "swarm") code = compose ? info.swarm.stackFile : info.swarm.command;
  else if (compose) code = info.standalone.composeFile;

  return (
    <div className="flex flex-col gap-3">
      <ViewToggle value={view} onChange={onViewChange} />
      <CopyableCode
        code={code}
        secret={info.joinToken}
        label={t(compose ? "install.copyLabel.composeFile" : "install.copyLabel.command")}
      />
      <p className="text-sm text-muted-foreground">
        <TickedText text={t(helperKey(kind, compose))} />
        {kind === "swarm" && compose ? <> {t("install.helper.composeSwarmSecret")}</> : null}
      </p>
    </div>
  );
}

function helperKey(kind: EnvKind, compose: boolean): string {
  if (kind === "swarm") return compose ? "install.helper.composeSwarm" : "install.helper.swarm";
  return compose ? "install.helper.composeStandalone" : "install.helper.standalone";
}

interface InstallStepProps {
  info: InstallInfo;
  envId: string;
  envName?: string;
  envType?: EnvKind;
  hasAgents: boolean;
  onClose: () => void;
}

// InstallStep is step 2: tabs, the masked command or file, the key-value list
// and the secret notice. A new environment does not know its type yet, so
// Swarm is first; one that has agents shows only its own type's tab, because
// the type is locked once an agent has enrolled (D-11).
function InstallStep({ info, envId, envName, envType, hasAgents, onClose }: InstallStepProps) {
  const t = useT();
  const locked: EnvKind | undefined = hasAgents ? envType : undefined;
  const kinds: EnvKind[] = locked ? [locked] : ["swarm", "standalone"];
  const [tab, setTab] = useState<EnvKind>(kinds[0] ?? "swarm");
  const [view, setView] = useState<View>("command");
  const update = hasAgents && envType === "swarm" && Boolean(info.swarm.rotateCommand);

  return (
    <div className="flex min-w-0 flex-col gap-4">
      <Tabs
        value={tab}
        onValueChange={(next) => setTab(next === "standalone" ? "standalone" : "swarm")}
      >
        <TabsList aria-label={t("install.tabs.label")} className="max-md:h-11">
          {kinds.map((kind) => (
            <TabsTrigger key={kind} value={kind}>
              {t(`install.tabs.${kind}`)}
            </TabsTrigger>
          ))}
        </TabsList>
        {kinds.map((kind) => (
          <TabsContent key={kind} value={kind} className="min-w-0 pt-2">
            <TabBody
              kind={kind}
              info={info}
              view={view}
              onViewChange={setView}
              update={update && kind === "swarm"}
            />
          </TabsContent>
        ))}
      </Tabs>

      <dl
        aria-label={t("install.details.label")}
        className="grid grid-cols-1 gap-x-4 gap-y-0.5 text-xs sm:grid-cols-[max-content_1fr] sm:gap-y-1"
      >
        <dt className="text-muted-foreground">{t("install.details.image")}</dt>
        <dd className="mb-1.5 font-mono break-all sm:mb-0">{info.image}</dd>
        <dt className="text-muted-foreground">{t("install.details.connectsTo")}</dt>
        <dd className="mb-1.5 flex min-w-0 flex-wrap items-baseline gap-x-3 sm:mb-0">
          <span className="font-mono break-all">{info.agentUrl}</span>
          <Link to="/settings/general" className="underline underline-offset-3" onClick={onClose}>
            {t("install.details.changeUrl")}
          </Link>
        </dd>
        {info.pin ? (
          <>
            <dt className="text-muted-foreground">{t("install.details.pin")}</dt>
            <dd className="font-mono break-all">{info.pin}</dd>
          </>
        ) : null}
      </dl>

      <Alert variant="warning" role="note">
        <AlertDescription>{t("install.secretNotice")}</AlertDescription>
      </Alert>

      {/* An environment that already has agents has nothing to wait for. */}
      {hasAgents ? null : <WaitingPanel envId={envId} envName={envName} agentUrl={info.agentUrl} />}
    </div>
  );
}

interface NameFormProps {
  pending: boolean;
  formError: string | null;
  onSubmit: (name: string) => void;
  onCancel: () => void;
}

// NameForm is step 1 of the add flow: an optional name and the one primary
// button. The field and the buttons are disabled while the call runs.
function NameForm({ pending, formError, onSubmit, onCancel }: NameFormProps) {
  const t = useT();
  const form = useForm<NameValues>({
    resolver: zodResolver(nameSchema),
    defaultValues: { name: "" },
    mode: "onSubmit",
  });
  const message = form.formState.errors.name?.message;
  const error = typeof message === "string" ? t(message) : undefined;
  const register = form.register("name");

  return (
    <form
      noValidate
      onSubmit={form.handleSubmit((values) => onSubmit(values.name.trim()))}
      className="flex flex-col gap-4"
    >
      {formError ? <FormFailure error={formError} /> : null}
      <fieldset disabled={pending} className="contents">
        <FieldGroup className="gap-4">
          <Field data-invalid={error ? true : undefined}>
            <FieldLabel htmlFor="install-name">{t("install.name.label")}</FieldLabel>
            <Input
              id="install-name"
              {...register}
              autoComplete="off"
              spellCheck={false}
              aria-invalid={Boolean(error)}
              aria-describedby={
                error ? "install-name-helper install-name-error" : "install-name-helper"
              }
              className="max-md:h-11"
            />
            <FieldDescription id="install-name-helper">{t("install.name.helper")}</FieldDescription>
            {error ? <FieldError id="install-name-error">{error}</FieldError> : null}
          </Field>
        </FieldGroup>
        <DialogFooter>
          <Button type="button" variant="outline" className={TOUCH_HEIGHT} onClick={onCancel}>
            {t("install.cancel")}
          </Button>
          <Button type="submit" className={TOUCH_HEIGHT}>
            {pending ? (
              <>
                <Spinner aria-hidden="true" />
                {t("install.add.pending")}
              </>
            ) : (
              t("install.add.submit")
            )}
          </Button>
        </DialogFooter>
      </fieldset>
    </form>
  );
}

// FormFailure is the form-level role=alert for a failed call (UI-SPEC UI
// Considerations: install dialog creation or rotation fails).
function FormFailure({ error }: { error: string }) {
  const t = useT();
  if (error === "publicUrlRequired") {
    return (
      <Alert variant="destructive" role="alert">
        <AlertDescription>
          {t("install.errors.publicUrlRequired.before")}{" "}
          <Link to="/settings/general" className="underline underline-offset-3">
            {t("install.errors.publicUrlRequired.link")}
          </Link>
          {t("install.errors.publicUrlRequired.after")}
        </AlertDescription>
      </Alert>
    );
  }
  return (
    <Alert variant="destructive" role="alert">
      <AlertDescription>{t(`install.errors.${error}`)}</AlertDescription>
    </Alert>
  );
}

// InstallFlow is what is inside the dialog. It is mounted only while the dialog
// is open, so closing it drops the step, and the join token with it, along
// with the mutations that fetched it (they have no cache time either).
function InstallFlow({ mode, envId, onClose, onBusyChange, initialFocus }: FlowProps) {
  const t = useT();
  const queryClient = useQueryClient();
  const [step, setStep] = useState<Step>({ kind: mode === "add" ? "name" : "confirm" });
  const [formError, setFormError] = useState<string | null>(null);
  const titleRef = useRef<HTMLHeadingElement>(null);
  const busy = useRef(false);

  // gcTime 0: a finished mutation is forgotten as soon as nothing watches it,
  // so the answer with the token is never kept in the cache.
  const create = useMutation({ mutationFn: createAgentEnvironment, gcTime: 0 });
  const rotate = useMutation({ mutationFn: rotateToken, gcTime: 0 });
  const pending = create.isPending || rotate.isPending;

  useEffect(() => {
    onBusyChange(pending);
    return () => onBusyChange(false);
  }, [pending, onBusyChange]);

  // Focus goes to the step 2 heading when the command appears (UI-SPEC).
  const installed = step.kind === "install";
  useEffect(() => {
    if (installed) titleRef.current?.focus();
  }, [installed]);

  async function generate(run: () => Promise<Step>, fallback: "create" | "rotate") {
    if (busy.current) return;
    busy.current = true;
    setFormError(null);
    try {
      setStep(await run());
    } catch (error) {
      setFormError(failureKey(error, fallback));
    } finally {
      busy.current = false;
    }
  }

  function createEnvironment(name: string) {
    void generate(async () => {
      const created = await create.mutateAsync(name === "" ? undefined : name);
      // The overview behind the dialog gets the new pending row.
      void queryClient.invalidateQueries({ queryKey: environmentsQuery.queryKey });
      return {
        kind: "install",
        install: created.install,
        envId: created.id,
        envName: created.name,
        hasAgents: false,
      };
    }, "create");
  }

  function rotateEnvironment() {
    if (!envId) return;
    void generate(async () => {
      const answer = await rotate.mutateAsync(envId);
      return {
        kind: "install",
        install: answer.install,
        envId,
        envType: answer.envType,
        hasAgents: answer.hasAgents,
      };
    }, "rotate");
  }

  const title = headingKey(step);
  return (
    <>
      <DialogHeader>
        <DialogTitle ref={titleRef} tabIndex={-1} className="outline-none">
          {t(title)}
        </DialogTitle>
        <DialogDescription>{t(descriptionKey(step))}</DialogDescription>
      </DialogHeader>

      {step.kind === "name" ? (
        <NameForm
          pending={pending}
          formError={formError}
          onSubmit={createEnvironment}
          onCancel={onClose}
        />
      ) : null}

      {step.kind === "confirm" ? (
        <div className="flex flex-col gap-4">
          <Alert variant="warning" role="alert">
            <AlertDescription>{t("install.rotate.alert")}</AlertDescription>
          </Alert>
          {formError ? <FormFailure error={formError} /> : null}
          <DialogFooter>
            <Button
              ref={initialFocus as RefObject<HTMLButtonElement>}
              type="button"
              variant="outline"
              className={TOUCH_HEIGHT}
              disabled={pending}
              onClick={onClose}
            >
              {t("install.cancel")}
            </Button>
            <Button
              type="button"
              className={TOUCH_HEIGHT}
              disabled={pending}
              onClick={rotateEnvironment}
            >
              {pending ? (
                <>
                  <Spinner aria-hidden="true" />
                  {t("install.rotate.pending")}
                </>
              ) : (
                t("install.rotate.submit")
              )}
            </Button>
          </DialogFooter>
        </div>
      ) : null}

      {step.kind === "install" ? (
        <>
          <InstallStep
            info={step.install}
            envId={step.envId}
            envName={step.envName}
            envType={step.envType}
            hasAgents={step.hasAgents}
            onClose={onClose}
          />
          <DialogFooter>
            <Button type="button" variant="outline" className={TOUCH_HEIGHT} onClick={onClose}>
              {t("install.close")}
            </Button>
          </DialogFooter>
        </>
      ) : null}
    </>
  );
}

function headingKey(step: Step): string {
  if (step.kind === "name") return "install.add.title";
  if (step.kind === "confirm") return "install.rotate.title";
  return "install.step2.title";
}

function descriptionKey(step: Step): string {
  if (step.kind === "name") return "install.add.body";
  if (step.kind === "confirm") return "install.rotate.description";
  return "install.step2.body";
}

export interface InstallDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  // add: step 1 asks for a name and creates the environment. rotate: opens on
  // the confirm for the environment envId, and a new token is generated only
  // when it is pressed.
  mode: "add" | "rotate";
  envId?: string;
  // What takes focus when the dialog closes. A dialog with no Radix trigger
  // (this one is opened from state) otherwise leaves focus on the page body.
  returnFocusRef?: RefObject<HTMLElement | null>;
}

// InstallDialog is the one surface for "Add environment", "Show install
// command" and "Finish setup" (D-15, D-23). The join token is shown once, masked
// until asked, and lives only in the component state of the open dialog: it is
// not in a URL, the router, a storage area or the query cache (D-20).
export function InstallDialog({
  open,
  onOpenChange,
  mode,
  envId,
  returnFocusRef,
}: InstallDialogProps) {
  const [busy, setBusy] = useState(false);
  const initialFocus = useRef<HTMLElement | null>(null);

  // A close while a call is running would hide a token nobody has seen.
  function change(next: boolean) {
    if (!next && busy) return;
    onOpenChange(next);
  }

  return (
    <Dialog open={open} onOpenChange={change}>
      <DialogContent
        showCloseButton={false}
        className="max-h-[90svh] overflow-y-auto sm:max-w-160"
        onOpenAutoFocus={(event) => {
          // The confirm starts on Cancel, as every destructive dialog does.
          if (!initialFocus.current) return;
          event.preventDefault();
          initialFocus.current.focus();
        }}
        onCloseAutoFocus={(event) => {
          if (!returnFocusRef?.current) return;
          event.preventDefault();
          returnFocusRef.current.focus();
        }}
      >
        {open ? (
          <InstallFlow
            mode={mode}
            envId={envId}
            onClose={() => change(false)}
            onBusyChange={setBusy}
            initialFocus={initialFocus}
          />
        ) : null}
      </DialogContent>
    </Dialog>
  );
}
