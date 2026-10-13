// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { zodResolver } from "@hookform/resolvers/zod";
import { useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { Eye, EyeOff, Lock } from "lucide-react";
import { type ReactNode, useEffect, useRef, useState } from "react";
import { type FieldErrors, useForm } from "react-hook-form";
import { useTranslation } from "react-i18next";

import { api } from "@/api/client";
import { readProblem } from "@/api/problem";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Field, FieldDescription, FieldError, FieldGroup, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import {
  InputGroup,
  InputGroupAddon,
  InputGroupButton,
  InputGroupInput,
} from "@/components/ui/input-group";
import { Spinner } from "@/components/ui/spinner";
import { WizardStepper } from "@/components/wizard-stepper";
import {
  type AdminInput,
  type AdminValues,
  adminSchema,
  type InstanceInput,
  type InstanceValues,
  instanceSchema,
  lowerAscii,
  MAX_INSTANCE_NAME_LENGTH,
  MAX_TOKEN_LENGTH,
  MAX_USERNAME_LENGTH,
  type RoutedError,
  routeServerFieldErrors,
  type SetupField,
  type TokenValues,
  tokenFormSchema,
} from "@/features/setup/schemas";
import { SetupRestore } from "@/features/setup/setup-restore";
import { basePath } from "@/lib/base-path";
import { cn } from "@/lib/utils";

type Step = 1 | 2 | 3;

const UNREACHABLE = "common.errors.unreachable";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";
const PRIMARY = cn("w-full", TOUCH_HEIGHT);

// None of the wizard's values (the setup token, the password, the instance
// settings) is written to any browser storage API, a cookie or the URL.
// They live in react-hook-form's in-memory state and the component state below
// and disappear with the page (T-01-47).

interface FieldShellProps {
  id: string;
  label: string;
  helper?: ReactNode;
  error?: string;
  children: (a: { id: string; "aria-invalid": boolean; "aria-describedby": string }) => ReactNode;
}

// FieldShell links a visible label, helper text and an inline error to one
// control: the control gets aria-invalid and an aria-describedby naming the
// helper and, while there is one, the error (UI-SPEC "Forms").
function FieldShell({ id, label, helper, error, children }: FieldShellProps) {
  const helperId = `${id}-helper`;
  const errorId = `${id}-error`;
  const describedBy = [helper ? helperId : null, error ? errorId : null].filter(Boolean).join(" ");
  return (
    <Field data-invalid={error ? true : undefined}>
      <FieldLabel htmlFor={id}>{label}</FieldLabel>
      {children({ id, "aria-invalid": Boolean(error), "aria-describedby": describedBy })}
      {helper ? <FieldDescription id={helperId}>{helper}</FieldDescription> : null}
      {error ? <FieldError id={errorId}>{error}</FieldError> : null}
    </Field>
  );
}

export function SetupWizard() {
  const { t } = useTranslation(["setup", "common"]);
  const navigate = useNavigate();
  const queryClient = useQueryClient();

  const [step, setStep] = useState<Step>(1);
  // The token the server accepted. Held in state only, and sent again with the
  // final request because the server checks it again there.
  const [verifiedToken, setVerifiedToken] = useState<string | null>(null);
  const [urlLocked, setUrlLocked] = useState(false);
  const [showPassword, setShowPassword] = useState(false);
  const [pending, setPending] = useState(false);
  const [formError, setFormError] = useState<string | null>(null);
  // The restore branch (D-30) takes the place of the admin step. The admin form
  // keeps its values while it is hidden, so Back returns to what was typed.
  const [restoring, setRestoring] = useState(false);

  const submitting = useRef(false);
  const headingRef = useRef<HTMLHeadingElement>(null);
  const shownStep = useRef<Step>(step);
  const pendingFocus = useRef<{ field: SetupField; step: Step } | null>(null);

  const tokenForm = useForm<TokenValues>({
    resolver: zodResolver(tokenFormSchema),
    mode: "onBlur",
    defaultValues: { token: "" },
  });
  const adminForm = useForm<AdminInput, unknown, AdminValues>({
    resolver: zodResolver(adminSchema),
    mode: "onBlur",
    defaultValues: { username: "", password: "", confirm: "" },
  });
  const instanceForm = useForm<InstanceInput, unknown, InstanceValues>({
    resolver: zodResolver(instanceSchema),
    mode: "onBlur",
    defaultValues: { instanceName: "", publicUrl: window.location.origin + basePath() },
  });

  // Every key starts with its namespace ("setup.errors.x", "common.errors.x").
  const tr = (key: string) => t(key, { ns: key.split(".")[0] });
  const errorOf = (errors: FieldErrors, name: string) => {
    const message = errors[name]?.message;
    return typeof message === "string" ? tr(message) : undefined;
  };

  // Focus follows the screen once the request is over and the fields are
  // enabled again: to the field a server error points at, otherwise to the new
  // step's heading when the step changed (UI-SPEC "Auth screens").
  // biome-ignore lint/correctness/useExhaustiveDependencies: runs on step and pending changes only
  useEffect(() => {
    if (pending) return;
    const target = pendingFocus.current;
    if (target) {
      // React can commit the end of the request before the jump back to the
      // field's step. Focusing then would reach a field that is not on screen
      // yet, so the focus waits until its step is the one shown.
      if (target.step !== step) return;
      pendingFocus.current = null;
      shownStep.current = step;
      focusField(target.field);
    } else if (shownStep.current !== step) {
      shownStep.current = step;
      headingRef.current?.focus();
    }
  }, [step, pending]);

  function focusField(field: SetupField) {
    if (field === "token") tokenForm.setFocus("token");
    else if (field === "username" || field === "password") adminForm.setFocus(field);
    else instanceForm.setFocus(field);
  }

  function goTo(next: Step) {
    setFormError(null);
    setStep(next);
  }

  // run guards a request against a double submit and always clears the pending
  // state, whatever the request does.
  async function run(request: () => Promise<void>) {
    if (submitting.current) return;
    submitting.current = true;
    setPending(true);
    setFormError(null);
    try {
      await request();
    } catch {
      setFormError(UNREACHABLE);
    } finally {
      submitting.current = false;
      setPending(false);
    }
  }

  // showServerErrors puts a problem where it belongs: field errors on the step
  // that owns the earliest field, a form-level alert for everything else.
  function showServerErrors(errors: RoutedError[]) {
    const first = errors[0];
    if (!first || first.step === null || !first.field) {
      setFormError(first?.messageKey ?? "setup.errors.server");
      return;
    }
    for (const e of errors) {
      if (e.step !== first.step || !e.field) continue;
      const opts = { type: "server", message: e.messageKey };
      if (e.field === "token") tokenForm.setError("token", opts);
      else if (e.field === "username" || e.field === "password") adminForm.setError(e.field, opts);
      else instanceForm.setError(e.field, opts);
    }
    // Focus waits for the pending state to clear (see the effect above), which
    // also covers a jump to an earlier step.
    pendingFocus.current = { field: first.field, step: first.step };
    if (first.step !== step) setStep(first.step);
  }

  const onToken = ({ token }: TokenValues) =>
    run(async () => {
      // Back from step 2 with the same token: it is already verified.
      if (token === verifiedToken) {
        goTo(2);
        return;
      }
      const { data, error, response } = await api.POST("/api/setup/token", { body: { token } });
      if (data) {
        setVerifiedToken(token);
        setUrlLocked(data.publicUrlLocked);
        // The server hostname and the address the browser used are only
        // defaults: anything the user already typed stays.
        instanceForm.reset(
          {
            instanceName: data.defaultInstanceName,
            publicUrl: data.publicUrl || window.location.origin + basePath(),
          },
          { keepDirtyValues: true },
        );
        goTo(2);
        return;
      }
      if (response.status === 409) {
        await navigate({ to: "/" });
        return;
      }
      showServerErrors(routeServerFieldErrors(readProblem(error, response)));
    });

  const onAdmin = () => goTo(3);

  const onComplete = (instance: InstanceValues) =>
    run(async () => {
      const admin = adminSchema.safeParse(adminForm.getValues());
      if (!admin.success || verifiedToken === null) {
        // Cannot happen after the step validations; fail towards the step that
        // can fix it rather than sending a half-valid request.
        goTo(admin.success ? 1 : 2);
        return;
      }
      const { data, error, response } = await api.POST("/api/setup/complete", {
        body: {
          token: verifiedToken,
          username: admin.data.username,
          password: admin.data.password,
          instanceName: instance.instanceName,
          publicUrl: instance.publicUrl,
        },
      });
      if (data) {
        // The browser now holds the session cookie. The instance no longer
        // needs setup, so refresh the query before leaving.
        await queryClient.invalidateQueries({ queryKey: ["instance"] });
        // Step 4 (the local Docker host) is its own signed-in page (UI-SPEC
        // interpretation 1). A session started by this request has no cached
        // /api/me answer yet, so the route loads it.
        await navigate({ to: "/welcome" });
        return;
      }
      if (response.status === 409) {
        await navigate({ to: "/" });
        return;
      }
      showServerErrors(routeServerFieldErrors(readProblem(error, response)));
    });

  const inRestore = step === 2 && restoring;
  const heading =
    step === 1
      ? "setup.token.heading"
      : inRestore
        ? "setup.restore.heading"
        : step === 2
          ? "setup.admin.heading"
          : "setup.instance.heading";
  const body =
    step === 1
      ? "setup.token.body"
      : inRestore
        ? "setup.restore.body"
        : step === 2
          ? "setup.admin.body"
          : "setup.instance.body";

  const pendingLabel = (key: string) => (
    <>
      <Spinner aria-label={t(key)} />
      {t(key)}
    </>
  );

  // The command the helper names stays code; the sentence around it is the
  // translated part, split at the placeholder.
  const [helperLead = "", helperTail = ""] = t("setup.token.helper", {
    command: "\u0000",
  }).split("\u0000");

  const username = adminForm.register("username");
  const password = adminForm.register("password", { deps: ["confirm"] });
  // Leaving the password field checks the confirmation as well, and an empty one
  // does not match. Once the field shows an error it is re-checked on every
  // keystroke, so the error goes as soon as the right text is typed. Waiting for
  // the blur would remove it between mouse-down and mouse-up on Continue, move the
  // buttons up and lose the click.
  const confirm = adminForm.register("confirm", {
    onChange: () => {
      if (adminForm.getFieldState("confirm").error) void adminForm.trigger("confirm");
    },
  });

  return (
    <div className="flex flex-col gap-4">
      <WizardStepper current={step} total={inRestore ? 2 : 4} />
      <div className="flex flex-col gap-1">
        <h2
          ref={headingRef}
          tabIndex={-1}
          className="text-base leading-tight font-medium outline-none"
        >
          {t(heading)}
        </h2>
        <p className="text-sm text-muted-foreground">{t(body)}</p>
      </div>

      {formError ? (
        <Alert variant="destructive">
          <AlertDescription>{tr(formError)}</AlertDescription>
        </Alert>
      ) : null}

      {step === 1 ? (
        <form noValidate onSubmit={tokenForm.handleSubmit(onToken)}>
          <fieldset disabled={pending} className="contents">
            <FieldGroup className="gap-4">
              <FieldShell
                id="setup-token"
                label={t("setup.token.label")}
                helper={
                  <>
                    {helperLead}
                    <code className="font-mono text-foreground">{t("setup.token.command")}</code>
                    {helperTail}
                  </>
                }
                error={errorOf(tokenForm.formState.errors, "token")}
              >
                {(a) => (
                  <Input
                    {...a}
                    {...tokenForm.register("token")}
                    className={cn("font-mono", TOUCH_HEIGHT)}
                    autoComplete="off"
                    autoCapitalize="off"
                    autoCorrect="off"
                    spellCheck={false}
                    maxLength={MAX_TOKEN_LENGTH}
                  />
                )}
              </FieldShell>
              <Button type="submit" className={PRIMARY}>
                {pending ? pendingLabel("setup.token.pending") : t("setup.token.continue")}
              </Button>
            </FieldGroup>
          </fieldset>
        </form>
      ) : null}

      {inRestore && verifiedToken !== null ? (
        <SetupRestore token={verifiedToken} onBack={() => setRestoring(false)} />
      ) : null}

      {step === 2 && !restoring ? (
        <form noValidate onSubmit={adminForm.handleSubmit(onAdmin)}>
          <fieldset disabled={pending} className="contents">
            <FieldGroup className="gap-4">
              <FieldShell
                id="setup-username"
                label={t("setup.admin.username.label")}
                helper={t("setup.admin.username.helper")}
                error={errorOf(adminForm.formState.errors, "username")}
              >
                {(a) => (
                  <Input
                    {...a}
                    {...username}
                    onBlur={(e) => {
                      // Not case-sensitive (D-35): show what will be stored.
                      adminForm.setValue("username", lowerAscii(e.target.value), {
                        shouldDirty: true,
                      });
                      return username.onBlur(e);
                    }}
                    className={TOUCH_HEIGHT}
                    autoComplete="username"
                    autoCapitalize="off"
                    autoCorrect="off"
                    spellCheck={false}
                    maxLength={MAX_USERNAME_LENGTH}
                  />
                )}
              </FieldShell>
              <FieldShell
                id="setup-password"
                label={t("setup.admin.password.label")}
                helper={t("setup.admin.password.helper")}
                error={errorOf(adminForm.formState.errors, "password")}
              >
                {(a) => (
                  <InputGroup className={cn("md:h-8", "h-11")}>
                    <InputGroupInput
                      {...a}
                      {...password}
                      type={showPassword ? "text" : "password"}
                      autoComplete="new-password"
                      className="h-full"
                    />
                    <InputGroupAddon align="inline-end">
                      <InputGroupButton
                        size="icon-xs"
                        aria-label={t(showPassword ? "setup.admin.hide" : "setup.admin.show")}
                        onClick={() => setShowPassword((v) => !v)}
                      >
                        {showPassword ? <EyeOff aria-hidden="true" /> : <Eye aria-hidden="true" />}
                      </InputGroupButton>
                    </InputGroupAddon>
                  </InputGroup>
                )}
              </FieldShell>
              <FieldShell
                id="setup-confirm"
                label={t("setup.admin.confirm.label")}
                error={errorOf(adminForm.formState.errors, "confirm")}
              >
                {(a) => (
                  <Input
                    {...a}
                    {...confirm}
                    type={showPassword ? "text" : "password"}
                    autoComplete="new-password"
                    className={TOUCH_HEIGHT}
                  />
                )}
              </FieldShell>
              <div className="flex flex-col gap-2">
                <Button type="submit" className={PRIMARY}>
                  {t("setup.admin.continue")}
                </Button>
                <Button type="button" variant="ghost" className={PRIMARY} onClick={() => goTo(1)}>
                  {t("setup.admin.back")}
                </Button>
              </div>
              <p className="text-sm text-muted-foreground">
                {t("setup.admin.restorePrompt")}{" "}
                <Button
                  type="button"
                  variant="link"
                  className="h-auto p-0 align-baseline text-sm"
                  onClick={() => setRestoring(true)}
                >
                  {t("setup.admin.restoreLink")}
                </Button>
              </p>
            </FieldGroup>
          </fieldset>
        </form>
      ) : null}

      {step === 3 ? (
        <form noValidate onSubmit={instanceForm.handleSubmit(onComplete)}>
          <fieldset disabled={pending} className="contents">
            <FieldGroup className="gap-4">
              <FieldShell
                id="setup-instance-name"
                label={t("setup.instance.name.label")}
                helper={t("setup.instance.name.helper")}
                error={errorOf(instanceForm.formState.errors, "instanceName")}
              >
                {(a) => (
                  <Input
                    {...a}
                    {...instanceForm.register("instanceName")}
                    className={TOUCH_HEIGHT}
                    autoComplete="off"
                    maxLength={MAX_INSTANCE_NAME_LENGTH}
                  />
                )}
              </FieldShell>
              <FieldShell
                id="setup-public-url"
                label={t("setup.instance.url.label")}
                helper={urlLocked ? t("setup.instance.url.locked") : t("setup.instance.url.helper")}
                error={errorOf(instanceForm.formState.errors, "publicUrl")}
              >
                {(a) =>
                  urlLocked ? (
                    <InputGroup className="h-11 md:h-8">
                      <InputGroupAddon>
                        <Lock role="img" aria-label={t("setup.instance.url.lockedIcon")} />
                      </InputGroupAddon>
                      <InputGroupInput
                        {...a}
                        {...instanceForm.register("publicUrl")}
                        readOnly
                        inputMode="url"
                        className="h-full font-mono"
                      />
                    </InputGroup>
                  ) : (
                    <Input
                      {...a}
                      {...instanceForm.register("publicUrl")}
                      inputMode="url"
                      autoComplete="off"
                      autoCapitalize="off"
                      autoCorrect="off"
                      spellCheck={false}
                      className={cn("font-mono", TOUCH_HEIGHT)}
                    />
                  )
                }
              </FieldShell>
              <div className="flex flex-col gap-2">
                <Button type="submit" className={PRIMARY}>
                  {pending ? pendingLabel("setup.instance.pending") : t("setup.instance.submit")}
                </Button>
                <Button type="button" variant="ghost" className={PRIMARY} onClick={() => goTo(2)}>
                  {t("setup.instance.back")}
                </Button>
              </div>
            </FieldGroup>
          </fieldset>
        </form>
      ) : null}
    </div>
  );
}
