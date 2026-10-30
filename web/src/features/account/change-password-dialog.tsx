// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { zodResolver } from "@hookform/resolvers/zod";
import { useQueryClient } from "@tanstack/react-query";
import { Eye, EyeOff } from "lucide-react";
import { type ReactNode, type RefObject, useRef, useState } from "react";
import { useForm } from "react-hook-form";
import { api, setCsrfToken } from "@/api/client";
import { readProblem } from "@/api/problem";
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
import {
  InputGroup,
  InputGroupAddon,
  InputGroupButton,
  InputGroupInput,
} from "@/components/ui/input-group";
import { Spinner } from "@/components/ui/spinner";
import { PASSWORD_KEYS, passwordSchema } from "@/features/setup/schemas";
import { type Me, meQueryOptions } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { toastSuccess } from "@/lib/toast";
import { z } from "@/lib/zod-config";

// Validation here is UX only: the server checks the current password and the
// whole policy again. Messages are i18n keys, never literals (D-65). The new
// password reuses the wizard's rule and messages, so both screens say the same.
const KEY = {
  currentRequired: "shell.account.changePassword.errors.currentRequired",
  currentIncorrect: "shell.account.changePassword.errors.currentIncorrect",
  server: "common.mutationFailed",
  unreachable: "common.errors.unreachable",
  rateLimited: "setup.errors.rateLimited",
} as const;

const changePasswordSchema = z
  .object({
    current: z.string().min(1, { error: KEY.currentRequired }),
    next: passwordSchema,
    confirm: z.string(),
  })
  .refine((v) => v.next === v.confirm, { path: ["confirm"], error: PASSWORD_KEYS.mismatch });

type Values = z.input<typeof changePasswordSchema>;
type FieldName = keyof Values;

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

// The server's field codes, from POST /api/me/password.
const SERVER_CODES: Record<string, { field: FieldName; messageKey: string }> = {
  current_password_incorrect: { field: "current", messageKey: KEY.currentIncorrect },
  password_too_short: { field: "next", messageKey: PASSWORD_KEYS.short },
  password_common: { field: "next", messageKey: PASSWORD_KEYS.common },
};

interface PasswordFieldProps {
  id: string;
  name: FieldName;
  label: string;
  helper?: string;
  error?: string;
  autoComplete: "current-password" | "new-password";
  showLabel: string;
  hideLabel: string;
  inputProps: object;
}

// PasswordField is a labelled password input with its own show/hide toggle. The
// control gets aria-invalid and an aria-describedby naming the helper and,
// while there is one, the error (UI-SPEC "Forms").
function PasswordField({
  id,
  name,
  label,
  helper,
  error,
  autoComplete,
  showLabel,
  hideLabel,
  inputProps,
}: PasswordFieldProps) {
  const [shown, setShown] = useState(false);
  const helperId = `${id}-helper`;
  const errorId = `${id}-error`;
  const describedBy = [helper ? helperId : null, error ? errorId : null].filter(Boolean).join(" ");
  let toggleIcon: ReactNode = <Eye aria-hidden="true" />;
  if (shown) toggleIcon = <EyeOff aria-hidden="true" />;
  return (
    <Field data-invalid={error ? true : undefined}>
      <FieldLabel htmlFor={id}>{label}</FieldLabel>
      <InputGroup className="h-11 md:h-8">
        <InputGroupInput
          id={id}
          name={name}
          {...inputProps}
          type={shown ? "text" : "password"}
          autoComplete={autoComplete}
          aria-invalid={Boolean(error)}
          aria-describedby={describedBy || undefined}
          className="h-full"
        />
        <InputGroupAddon align="inline-end">
          <InputGroupButton
            size="icon-xs"
            aria-label={shown ? hideLabel : showLabel}
            onClick={() => setShown((v) => !v)}
          >
            {toggleIcon}
          </InputGroupButton>
        </InputGroupAddon>
      </InputGroup>
      {helper ? <FieldDescription id={helperId}>{helper}</FieldDescription> : null}
      {error ? <FieldError id={errorId}>{error}</FieldError> : null}
    </Field>
  );
}

interface FormProps {
  onDone: () => void;
  onCancel: () => void;
  onPendingChange: (pending: boolean) => void;
}

// ChangePasswordForm lives inside the dialog content, so closing the dialog
// unmounts it and every typed password goes with it (T-01-60).
function ChangePasswordForm({ onDone, onCancel, onPendingChange }: FormProps) {
  const t = useT();
  const queryClient = useQueryClient();
  const [pending, setPending] = useState(false);
  const [formError, setFormError] = useState<string | null>(null);
  const submitting = useRef(false);

  const form = useForm<Values>({
    resolver: zodResolver(changePasswordSchema),
    defaultValues: { current: "", next: "", confirm: "" },
    mode: "onSubmit",
  });
  const errors = form.formState.errors;
  const errorOf = (name: FieldName) => {
    const message = errors[name]?.message;
    return typeof message === "string" ? t(message) : undefined;
  };

  function setBusy(busy: boolean) {
    submitting.current = busy;
    setPending(busy);
    onPendingChange(busy);
  }

  async function submit(values: Values) {
    if (submitting.current) return;
    setBusy(true);
    setFormError(null);
    try {
      const { data, error, response } = await api.POST("/api/me/password", {
        body: { currentPassword: values.current, newPassword: values.next },
      });
      if (data) {
        // The old session is gone and the server started a new one, so the
        // token that goes with it replaces the old one in memory and in the
        // cached answer, before anything is refetched (D-32, D-36).
        setCsrfToken(data.csrfToken);
        queryClient.setQueryData<Me>(meQueryOptions.queryKey, (me) =>
          me ? { ...me, csrfToken: data.csrfToken } : me,
        );
        void queryClient.invalidateQueries({ queryKey: meQueryOptions.queryKey });
        onDone();
        toastSuccess(t("shell.account.changePassword.success"));
        return;
      }
      showServerError(readProblem(error, response));
    } catch {
      setFormError(KEY.unreachable);
    } finally {
      setBusy(false);
    }
  }

  function showServerError(problem: ReturnType<typeof readProblem>) {
    const routed = problem.fields.flatMap((f) => {
      const known = SERVER_CODES[f.message];
      return known ? [known] : [];
    });
    if (problem.status === 429) {
      // The per-user throttle on attempts (T-01-60): the wizard's wording says it.
      setFormError(KEY.rateLimited);
      return;
    }
    if (problem.status !== 422 || routed.length === 0) {
      setFormError(KEY.server);
      return;
    }
    for (const r of routed) form.setError(r.field, { type: "server", message: r.messageKey });
    // Focus follows the first field the server pointed at.
    form.setFocus(routed[0]?.field ?? "current");
  }

  const current = form.register("current");
  const next = form.register("next", { deps: ["confirm"] });
  const confirm = form.register("confirm");
  const show = t("shell.account.changePassword.show");
  const hide = t("shell.account.changePassword.hide");

  return (
    <form noValidate onSubmit={form.handleSubmit(submit)} className="flex flex-col gap-4">
      {formError ? (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{t(formError)}</AlertDescription>
        </Alert>
      ) : null}
      <fieldset disabled={pending} className="contents">
        <FieldGroup className="gap-4">
          <PasswordField
            id="change-password-current"
            name="current"
            label={t("shell.account.changePassword.current.label")}
            error={errorOf("current")}
            autoComplete="current-password"
            showLabel={show}
            hideLabel={hide}
            inputProps={current}
          />
          <PasswordField
            id="change-password-new"
            name="next"
            label={t("shell.account.changePassword.new.label")}
            helper={t("shell.account.changePassword.new.helper")}
            error={errorOf("next")}
            autoComplete="new-password"
            showLabel={show}
            hideLabel={hide}
            inputProps={next}
          />
          <PasswordField
            id="change-password-confirm"
            name="confirm"
            label={t("shell.account.changePassword.confirm.label")}
            error={errorOf("confirm")}
            autoComplete="new-password"
            showLabel={show}
            hideLabel={hide}
            inputProps={confirm}
          />
          <FieldDescription>{t("shell.account.changePassword.helper")}</FieldDescription>
        </FieldGroup>
        <DialogFooter>
          <Button type="button" variant="outline" className={TOUCH_HEIGHT} onClick={onCancel}>
            {t("shell.account.changePassword.cancel")}
          </Button>
          <Button type="submit" className={TOUCH_HEIGHT}>
            {pending ? (
              <>
                <Spinner aria-hidden="true" />
                {t("shell.account.changePassword.pending")}
              </>
            ) : (
              t("shell.account.changePassword.submit")
            )}
          </Button>
        </DialogFooter>
      </fieldset>
    </form>
  );
}

interface ChangePasswordDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  // Where focus goes when the dialog closes: the control that opened it. The
  // menu item that did is gone by then, so Radix cannot find its own way back.
  returnFocusRef?: RefObject<HTMLElement | null>;
}

// ChangePasswordDialog is "Change password" from the user menu (D-34): the
// current password, then a new one that meets the D-07 policy. The dialog
// cannot be dismissed while a request is in flight.
export function ChangePasswordDialog({
  open,
  onOpenChange,
  returnFocusRef,
}: ChangePasswordDialogProps) {
  const t = useT();
  const [pending, setPending] = useState(false);

  return (
    <Dialog open={open} onOpenChange={(next) => (pending ? undefined : onOpenChange(next))}>
      <DialogContent
        showCloseButton={false}
        onCloseAutoFocus={(event) => {
          if (!returnFocusRef?.current) return;
          event.preventDefault();
          returnFocusRef.current.focus();
        }}
      >
        <DialogHeader>
          <DialogTitle>{t("shell.account.changePassword.title")}</DialogTitle>
          <DialogDescription>{t("shell.account.changePassword.description")}</DialogDescription>
        </DialogHeader>
        <ChangePasswordForm
          onDone={() => onOpenChange(false)}
          onCancel={() => onOpenChange(false)}
          onPendingChange={setPending}
        />
      </DialogContent>
    </Dialog>
  );
}
